package controller

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.universe.tf/metallb/api/v1beta1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/joeyloman/rancher-fip-lb-controller/pkg/ipam"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/purelb"
)

// MockIPAMClient is a mock of the IPAM client
type MockIPAMClient struct {
	RequestFIPFunc func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error)
	ReleaseFIPFunc func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error
	ListFIPsFunc   func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error)
}

func (m *MockIPAMClient) RequestFIP(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
	return m.RequestFIPFunc(ctx, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup)
}

func (m *MockIPAMClient) ReleaseFIP(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
	return m.ReleaseFIPFunc(ctx, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup)
}

func (m *MockIPAMClient) ListFIPs(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
	if m.ListFIPsFunc == nil {
		return &ipam.FIPListResponse{}, nil
	}
	return m.ListFIPsFunc(ctx, clientSecret, cluster, project, floatingIPPool)
}

// MockMetalLBClient is a mock of the MetalLB client
type MockMetalLBClient struct {
	CreateIPAddressPoolFunc   func(ctx context.Context, pool *v1beta1.IPAddressPool) error
	CreateL2AdvertisementFunc func(ctx context.Context, ad *v1beta1.L2Advertisement) error
	DeleteIPAddressPoolFunc   func(ctx context.Context, name, namespace string) error
	DeleteL2AdvertisementFunc func(ctx context.Context, name, namespace string) error
	GetIPAddressPoolsFunc     func(ctx context.Context, namespace string) ([]v1beta1.IPAddressPool, error)
	GetIPAddressPoolFunc      func(ctx context.Context, name, namespace string) (*v1beta1.IPAddressPool, error)
	UpdateIPAddressPoolFunc   func(ctx context.Context, pool *v1beta1.IPAddressPool) error
}

func (m *MockMetalLBClient) GetIPAddressPools(ctx context.Context, namespace string) ([]v1beta1.IPAddressPool, error) {
	return m.GetIPAddressPoolsFunc(ctx, namespace)
}

func (m *MockMetalLBClient) GetIPAddressPool(ctx context.Context, name, namespace string) (*v1beta1.IPAddressPool, error) {
	if m.GetIPAddressPoolFunc == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "metallb.io", Resource: "ipaddresspools"}, name)
	}
	return m.GetIPAddressPoolFunc(ctx, name, namespace)
}

func (m *MockMetalLBClient) CreateIPAddressPool(ctx context.Context, pool *v1beta1.IPAddressPool) error {
	return m.CreateIPAddressPoolFunc(ctx, pool)
}

func (m *MockMetalLBClient) CreateL2Advertisement(ctx context.Context, ad *v1beta1.L2Advertisement) error {
	return m.CreateL2AdvertisementFunc(ctx, ad)
}

func (m *MockMetalLBClient) DeleteIPAddressPool(ctx context.Context, name, namespace string) error {
	return m.DeleteIPAddressPoolFunc(ctx, name, namespace)
}

func (m *MockMetalLBClient) DeleteL2Advertisement(ctx context.Context, name, namespace string) error {
	return m.DeleteL2AdvertisementFunc(ctx, name, namespace)
}

func (m *MockMetalLBClient) UpdateIPAddressPool(ctx context.Context, pool *v1beta1.IPAddressPool) error {
	return m.UpdateIPAddressPoolFunc(ctx, pool)
}

// MockPureLBClient is a mock of the PureLB client
type MockPureLBClient struct {
	CreateServiceGroupFunc func(ctx context.Context, group *purelb.ServiceGroup) error
	CreateLBNodeAgentFunc  func(ctx context.Context, agent *purelb.LBNodeAgent) error
	DeleteServiceGroupFunc func(ctx context.Context, name, namespace string) error
	DeleteLBNodeAgentFunc  func(ctx context.Context, name, namespace string) error
	GetServiceGroupsFunc   func(ctx context.Context, namespace string) ([]purelb.ServiceGroup, error)
}

func (m *MockPureLBClient) CreateServiceGroup(ctx context.Context, group *purelb.ServiceGroup) error {
	return m.CreateServiceGroupFunc(ctx, group)
}

func (m *MockPureLBClient) CreateLBNodeAgent(ctx context.Context, agent *purelb.LBNodeAgent) error {
	return m.CreateLBNodeAgentFunc(ctx, agent)
}

func (m *MockPureLBClient) DeleteServiceGroup(ctx context.Context, name, namespace string) error {
	return m.DeleteServiceGroupFunc(ctx, name, namespace)
}

func (m *MockPureLBClient) DeleteLBNodeAgent(ctx context.Context, name, namespace string) error {
	return m.DeleteLBNodeAgentFunc(ctx, name, namespace)
}

func (m *MockPureLBClient) GetServiceGroups(ctx context.Context, namespace string) ([]purelb.ServiceGroup, error) {
	return m.GetServiceGroupsFunc(ctx, namespace)
}

// recordingRequeuer records the delayed reconciles a reconciler schedules.
type recordingRequeuer struct {
	keys   []interface{}
	delays []time.Duration
}

func (q *recordingRequeuer) AddAfter(item interface{}, duration time.Duration) {
	q.keys = append(q.keys, item)
	q.delays = append(q.delays, duration)
}

func TestController_reconcile(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			// The pending marker must already be persisted when the IPAM is
			// asked, so a crash from here on can adopt the allocation.
			pendingSvc, err := clientset.CoreV1().Services("test-ns").Get(ctx, "test-svc", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, "p-12345", pendingSvc.Annotations[floatingIPRequestAnnotation], "the pending marker must be written before RequestFIP")
			return "1.2.3.4", "1.2.3.4/24", "shared-key", nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			return nil
		},
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error {
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, secret, and configmap
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)

	// Check that the finalizer was added
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.Equal(t, "1.2.3.4/24", updatedSvc.Annotations["rancher.k8s.binbash.org/floatingip-subnet"])
	assert.Equal(t, "p-12345", updatedSvc.Annotations[projectAnnotation])
	assert.NotContains(t, updatedSvc.Annotations, floatingIPRequestAnnotation, "the pending marker must be cleared once the allocation is persisted")
}

func TestController_reconcile_ipv6_metallb_pool(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(10)

	var createdPool *v1beta1.IPAddressPool
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			return "2001:db8::1", "2001:db8::/64", "shared-key", nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			createdPool = pool
			return nil
		},
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error {
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	if assert.NotNil(t, createdPool, "the IPv6 pool must be created") {
		assert.Equal(t, []string{"2001:db8::1/128"}, createdPool.Spec.Addresses)
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "2001:db8::1", updatedSvc.Annotations[floatingIPAnnotation])
	assert.Equal(t, "2001:db8::/64", updatedSvc.Annotations["rancher.k8s.binbash.org/floatingip-subnet"])
}

func TestController_reconcile_delete(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	// Observe the progress annotation writes: a restart must be able to
	// resume after the pool deletion and after the release.
	var sawResourcesDeleted, sawReleased bool
	clientset.PrependReactor("update", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		update := action.(clienttesting.UpdateAction)
		if svc, ok := update.GetObject().(*v1.Service); ok {
			switch svc.Annotations[cleanupProgressAnnotation] {
			case cleanupStageResourcesDeleted:
				sawResourcesDeleted = true
			case cleanupStageReleased:
				sawReleased = true
			}
		}
		return false, nil, nil
	})

	ipamReleased := false
	var order []string
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			ipamReleased = true
			order = append(order, "release")
			return nil
		},
	}

	poolDeleted := false
	adDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			order = append(order, "pool")
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			order = append(order, "advertisement")
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, and secret
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			},
		},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)

	assert.True(t, ipamReleased, "IPAM FIP should be released")
	assert.True(t, poolDeleted, "IPAddressPool should be deleted")
	assert.True(t, adDeleted, "L2Advertisement should be deleted")
	assert.True(t, sawResourcesDeleted, "the cleanup must record that the resources were withdrawn")
	assert.True(t, sawReleased, "the cleanup must record that the address was released")

	// The address must stop being advertised before the IPAM may hand it to
	// another owner; releasing first opens a duplicate-IP window.
	assert.Equal(t, []string{"advertisement", "pool", "release"}, order)

	// Check that the finalizer was removed
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// Deleting a Service whose allocation was interrupted between RequestFIP and
// the annotation write must find the allocation in the IPAM and release it,
// instead of leaking it.
func TestController_reconcile_delete_releases_pending_allocation(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	releasedIP := ""
	mockIPAM := &MockIPAMClient{
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{{
				Cluster:          "c-12345",
				ServiceNamespace: "test-ns",
				ServiceName:      "test-svc",
				IPAddress:        "1.2.3.4",
			}}}, nil
		},
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			releasedIP = ipaddr
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc:   func(ctx context.Context, name, namespace string) error { return nil },
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error { return nil },
	}
	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           record.NewFakeRecorder(10),
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "test-ns",
		Labels: map[string]string{"field.cattle.io/projectId": "p-12345"},
	}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{floatingIPRequestAnnotation: "p-12345"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.Equal(t, "1.2.3.4", releasedIP, "the pending allocation must be released")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, floatingIPRequestAnnotation)
}

func TestController_reconcile_delete_with_shared_floatingip_group(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	ipamReleased := false
	releasedGroup := "unset"
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			ipamReleased = true
			releasedGroup = floatingIPGroup
			return nil
		},
	}

	poolDeleted := false
	adDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, and secret
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	// Create another service that shares the same floatingIPGroup and IP address
	now := metav1.Now()
	otherSvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "other-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				"rancher.k8s.binbash.org/floatingip":       "1.2.3.4",
				"rancher.k8s.binbash.org/floatingip-group": "shared-group",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			},
		},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), otherSvc, metav1.CreateOptions{})
	assert.NoError(t, err)

	// Create the service being deleted with the same floatingIPGroup and IP address
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				"rancher.k8s.binbash.org/floatingip":       "1.2.3.4",
				"rancher.k8s.binbash.org/floatingip-group": "shared-group",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			},
		},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)

	// The address is still used by another Service, so its pool and
	// advertisement must stay. This Service's own claim must still be
	// released: the manager removes only this Service's labels and keeps the
	// FloatingIP while the surviving member claims it. Skipping the release
	// would leave a stale claim that keeps the address allocated forever.
	assert.True(t, ipamReleased, "the deleting Service's own IPAM claim must be released even while another Service shares the address")
	assert.Equal(t, "", releasedGroup, "the release must identify the claim by Service and address, not by the editable group annotation")

	// IPAddressPool and L2Advertisement should NOT be deleted because another service shares the same floatingIPGroup and IP address
	assert.False(t, poolDeleted, "IPAddressPool should NOT be deleted when another service shares the same floatingIPGroup and IP address")
	assert.False(t, adDeleted, "L2Advertisement should NOT be deleted when another service shares the same floatingIPGroup and IP address")

	// Check that the finalizer was removed
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, "rancher.k8s.binbash.org/floatingip")
}

func TestController_reconcile_delete_without_floatingip_annotation(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	ipamReleased := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			ipamReleased = true
			return nil
		},
	}

	poolDeleted := false
	adDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)

	assert.False(t, ipamReleased, "IPAM FIP should not be released without floatingip annotation")
	assert.False(t, poolDeleted, "IPAddressPool should not be deleted without floatingip annotation")
	assert.False(t, adDeleted, "L2Advertisement should not be deleted without floatingip annotation")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

func TestController_reconcile_delete_without_project_removes_finalizer(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(10)

	ipamReleased := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			ipamReleased = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      &MockMetalLBClient{},
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	// Neither the app namespace nor the Service namespace belongs to a project.
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}}, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err, "deletion must reach a terminal decision without a project")
	assert.False(t, ipamReleased, "the address cannot be released without a project")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "OrphanedFloatingIP")
		assert.Contains(t, event, "1.2.3.4")
	default:
		t.Error("expected a Warning event reporting the orphaned FloatingIP")
	}
}

func TestController_reconcile_delete_with_missing_secret_removes_finalizer(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(10)

	ipamReleased := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			ipamReleased = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      &MockMetalLBClient{},
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	// The project was decommissioned: the namespace label is still there but
	// the config Secret is gone.
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err, "deletion must reach a terminal decision without the project Secret")
	assert.False(t, ipamReleased, "the address cannot be released without the project Secret")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "OrphanedFloatingIP")
		assert.Contains(t, event, "p-12345")
	default:
		t.Error("expected a Warning event reporting the orphaned FloatingIP")
	}
}

func TestController_reconcile_delete_uses_recorded_project(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(10)

	var releasedProject string
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			releasedProject = project
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc:   func(ctx context.Context, name, namespace string) error { return nil },
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error { return nil },
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	// The namespace now points at a different project than the one the address
	// was allocated under.
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-new"}}}, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-old", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-old"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				"rancher.k8s.binbash.org/floatingip":         "1.2.3.4",
				"rancher.k8s.binbash.org/floatingip-project": "p-old",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.Equal(t, "p-old", releasedProject, "deletion must use the project recorded at allocation time")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

func TestController_reconcile_no_secret(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	mockIPAM := &MockIPAMClient{}
	mockMetalLB := &MockMetalLBClient{}
	queue := &recordingRequeuer{}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		queue:              queue,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a namespace and service, but no secret
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err, "reconcile should not return an error when secret is not found")
	assert.Equal(t, []interface{}{"test-ns/test-svc"}, queue.keys, "the deferred decision must be retried")
	assert.Equal(t, []time.Duration{deferredRequeueDelay}, queue.delays)

	// Check that finalizer was NOT added
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

func TestController_reconcile_ipam_request_error(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", assert.AnError
		},
	}
	mockMetalLB := &MockMetalLBClient{}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, secret, and configmap
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.Error(t, err, "reconcile should return an error on IPAM request failure")
	assert.True(t, requestCalled, "RequestFIP should have been called")

	// Check that finalizer was NOT added
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

func TestController_reconcile_ipam_quota_exceeded(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", fmt.Errorf("quota exceeded")
		},
	}
	mockMetalLB := &MockMetalLBClient{}
	queue := &recordingRequeuer{}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		queue:              queue,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, secret, and configmap
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err, "reconcile should not return an error on quota exceeded")
	assert.True(t, requestCalled, "RequestFIP should have been called")
	assert.Equal(t, []interface{}{"test-ns/test-svc"}, queue.keys, "quota-exceeded must be retried instead of dropped")
	assert.Equal(t, []time.Duration{deferredRequeueDelay}, queue.delays)

	// Check that finalizer was NOT added
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

func TestController_reconcile_delete_ipam_release_error(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	ipamReleased := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			ipamReleased = true
			return assert.AnError
		},
	}

	poolDeleted := false
	adDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, and secret
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			},
		},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.Error(t, err)

	assert.True(t, ipamReleased, "IPAM ReleaseFIP should have been called")
	// The advertisement and pool are withdrawn before the release, so a
	// release failure leaves the Service protected by its finalizer with the
	// address no longer advertised. The retry tolerates the already-deleted
	// resources and only repeats the release.
	assert.True(t, poolDeleted, "IPAddressPool should already be deleted before the release")
	assert.True(t, adDeleted, "L2Advertisement should already be deleted before the release")

	// Check that the finalizer was NOT removed
	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

func TestController_reconcile_get_ip_address_pools(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	// Mock IPAM client that returns a successful FIP request
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			return "1.2.3.4", "1.2.3.4/24", "shared-key", nil
		},
	}

	// Track calls to GetIPAddressPools
	getPoolsCalled := false
	var getPoolsNamespace string
	createPoolCallCount := 0
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			createPoolCallCount++
			// Simulate CIDR overlap error on first call to trigger GetIPAddressPools call
			if createPoolCallCount == 1 {
				return fmt.Errorf("admission webhook \"ipaddresspoolvalidationwebhook.metallb.io\" denied the request: CIDR \"1.2.3.4/32\" in pool \"rancher-fip-test-ns-test-svc\" overlaps with already defined CIDR \"1.2.3.4/32\"")
			}
			// Return success on retry
			return nil
		},
		GetIPAddressPoolsFunc: func(ctx context.Context, namespace string) ([]v1beta1.IPAddressPool, error) {
			getPoolsCalled = true
			getPoolsNamespace = namespace
			// Return a pool with the overlapping CIDR
			return []v1beta1.IPAddressPool{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "existing-pool",
						Namespace: namespace,
					},
					Spec: v1beta1.IPAddressPoolSpec{
						Addresses: []string{"1.2.3.4/32"},
					},
				},
			}, nil
		},
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			return nil
		},
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error {
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
		caStore:            nil,
	}

	// Create a service, namespace, secret, and configmap
	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)

	// Verify that GetIPAddressPools was called
	assert.True(t, getPoolsCalled, "GetIPAddressPools should have been called")
	assert.Equal(t, "rancher-fip-manager", getPoolsNamespace, "GetIPAddressPools should be called with correct namespace")
}

// TestController_reconcile_adopts_existing_floatingip pins the adoption path:
// a Service that already carries the floatingip annotation must be reconciled
// from that allocation, not sent through RequestFIP again. The manager
// allocates a new address for a repeated request, so re-requesting after a
// restart or a retry would leak a FloatingIP per reconcile.
func TestController_reconcile_adopts_existing_floatingip(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "9.9.9.9", "9.9.9.9/24", "", nil
		},
	}

	var createdPool *v1beta1.IPAddressPool
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			createdPool = pool
			return nil
		},
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error {
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{})
	assert.NoError(t, err)

	// The Service was allocated before a controller restart: the annotation
	// is present, but there is no load balancer ingress yet.
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-svc",
			Namespace:   "test-ns",
			Annotations: map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requestCalled, "reconcile must not request a second FloatingIP for an already-allocated Service")

	if assert.NotNil(t, createdPool, "the pool for the adopted address must be reconciled") {
		assert.Equal(t, []string{"1.2.3.4/32"}, createdPool.Spec.Addresses)
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.Equal(t, "1.2.3.4", updatedSvc.Annotations["rancher.k8s.binbash.org/floatingip"])
	// An adopted allocation's project is unknown: recording the currently
	// derived project would freeze a guess that deletion would then trust.
	assert.NotContains(t, updatedSvc.Annotations, projectAnnotation)
}

// TestController_reconcile_adopts_own_static_ip pins the static-IP adoption
// path: a retry for a Service whose pinned address is already assigned to it
// must adopt that allocation instead of looping on the manager's 409 forever.
// The annotation may be missing because the crash happened before it was
// written.
func TestController_reconcile_adopts_own_static_ip(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			assert.Equal(t, "1.2.3.4", ipaddr)
			return "", "", "", fmt.Errorf("failed to request FIP: status code 409 ({\"message\":\"IP address is already assigned\"})")
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{
				FloatingIPs: []ipam.FloatingIP{{
					Project:          "p-12345",
					Cluster:          "c-12345",
					FloatingIPPool:   "pool1",
					ServiceNamespace: "test-ns",
					ServiceName:      "test-svc",
					IPAddress:        "1.2.3.4",
				}},
			}, nil
		},
	}

	var createdPool *v1beta1.IPAddressPool
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			createdPool = pool
			return nil
		},
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error {
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-svc",
			Namespace:   "test-ns",
			Annotations: map[string]string{"rancher.k8s.binbash.org/static-ip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err, "a pinned address owned by the Service must be adopted")
	if assert.NotNil(t, createdPool, "the pool for the adopted address must be reconciled") {
		assert.Equal(t, []string{"1.2.3.4/32"}, createdPool.Spec.Addresses)
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.Equal(t, "1.2.3.4", updatedSvc.Annotations["rancher.k8s.binbash.org/floatingip"])
}

// TestController_reconcile_static_ip_conflict_is_terminal pins the other half
// of the static-IP handling: when the pinned address belongs to another
// Service, the request can never succeed by retrying, so the reconcile
// reports a Warning and stops instead of re-queuing forever.
func TestController_reconcile_static_ip_conflict_is_terminal(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			return "", "", "", fmt.Errorf("failed to request FIP: status code 409 ({\"message\":\"IP address is already assigned\"})")
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{
				FloatingIPs: []ipam.FloatingIP{{
					Project:          "p-12345",
					Cluster:          "c-12345",
					FloatingIPPool:   "pool1",
					ServiceNamespace: "test-ns",
					ServiceName:      "other-svc",
					IPAddress:        "1.2.3.4",
				}},
			}, nil
		},
	}

	poolCreated := false
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			poolCreated = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-svc",
			Namespace:   "test-ns",
			Annotations: map[string]string{"rancher.k8s.binbash.org/static-ip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err, "a conflict with another Service must not be retried forever")
	assert.False(t, poolCreated, "no pool may be created for an address owned by another Service")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "StaticIPConflict")
	default:
		t.Error("expected a warning event about the static IP conflict")
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// TestController_reconcile_rejects_invalid_configuration pins the
// configuration validation: an unknown loadBalancerType or an incomplete
// project Secret must not reach the IPAM, because the address would be
// allocated and the Service annotated and finalized without any pool being
// created.
func TestController_reconcile_rejects_invalid_configuration(t *testing.T) {
	tests := []struct {
		name   string
		data   map[string][]byte
		wanted string
	}{
		{
			name:   "unknown loadBalancerType",
			data:   map[string][]byte{"loadBalancerType": []byte("nginx")},
			wanted: `unsupported loadBalancerType "nginx"`,
		},
		{
			name:   "empty loadBalancerType",
			data:   map[string][]byte{"loadBalancerType": nil},
			wanted: `unsupported loadBalancerType ""`,
		},
		{
			name:   "missing clientSecret",
			data:   map[string][]byte{"clientSecret": nil},
			wanted: `missing required key "clientSecret"`,
		},
		{
			name:   "missing floatingIPPool",
			data:   map[string][]byte{"floatingIPPool": nil},
			wanted: "no floatingIPPool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			recorder := record.NewFakeRecorder(10)

			requestCalls := 0
			mockIPAM := &MockIPAMClient{
				RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
					requestCalls++
					return "1.2.3.4", "1.2.3.4/24", "", nil
				},
			}
			poolCreated := false
			mockMetalLB := &MockMetalLBClient{
				CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
					poolCreated = true
					return nil
				},
			}

			r := &reconciler{
				clientset:          clientset,
				metallbClient:      mockMetalLB,
				ipamClientOverride: mockIPAM,
				logger:             logrus.New(),
				recorder:           recorder,
				appNamespace:       "rancher-fip-manager",
			}

			_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}, metav1.CreateOptions{})
			assert.NoError(t, err)
			_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}, metav1.CreateOptions{})
			assert.NoError(t, err)

			data := map[string][]byte{
				"apiUrl":           []byte("http://localhost"),
				"clientSecret":     []byte("secret"),
				"floatingIPPool":   []byte("pool1"),
				"cluster":          []byte("c-12345"),
				"project":          []byte("p-12345"),
				"loadBalancerType": []byte("metallb"),
			}
			for key, value := range tt.data {
				data[key] = value
			}
			_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
				Data:       data,
			}, metav1.CreateOptions{})
			assert.NoError(t, err)
			_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
				Data:       map[string]string{"pool1": "eth0"},
			}, metav1.CreateOptions{})
			assert.NoError(t, err)

			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
				Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
			}
			_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
			assert.NoError(t, err)

			err = r.reconcile(context.Background(), svc)
			assert.NoError(t, err, "an invalid configuration is reported, not retried as a failure")
			assert.Zero(t, requestCalls, "no address may be requested with an invalid configuration")
			assert.False(t, poolCreated, "no pool may be created with an invalid configuration")

			select {
			case event := <-recorder.Events:
				assert.Contains(t, event, "InvalidConfiguration")
				assert.Contains(t, event, tt.wanted)
			default:
				t.Error("expected a warning event about the invalid configuration")
			}

			updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Empty(t, updatedSvc.Annotations[floatingIPAnnotation])
			assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
		})
	}
}

// newPureLBAdoptionReconciler builds a reconciler for an already-annotated
// Service using the PureLB load balancer type.
func newPureLBAdoptionReconciler(t *testing.T, purelbClient purelbClient, recorder *record.FakeRecorder, annotations map[string]string) (*reconciler, *fake.Clientset, *v1.Service) {
	t.Helper()
	clientset := fake.NewSimpleClientset()
	r := &reconciler{
		clientset:          clientset,
		purelbClient:       purelbClient,
		ipamClientOverride: &MockIPAMClient{},
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("purelb"),
		},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns", Annotations: annotations},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)
	return r, clientset, svc
}

// TestController_reconcile_adopts_purelb_subnet pins the self-healing path for
// an adopted PureLB allocation: the subnet persisted at allocation time is
// used to rebuild a missing ServiceGroup.
func TestController_reconcile_adopts_purelb_subnet(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	var created *purelb.ServiceGroup
	mockPureLB := &MockPureLBClient{
		GetServiceGroupsFunc: func(ctx context.Context, namespace string) ([]purelb.ServiceGroup, error) {
			return nil, nil
		},
		CreateServiceGroupFunc: func(ctx context.Context, group *purelb.ServiceGroup) error {
			created = group
			return nil
		},
	}

	r, clientset, svc := newPureLBAdoptionReconciler(t, mockPureLB, recorder, map[string]string{
		"rancher.k8s.binbash.org/floatingip":        "1.2.3.4",
		"rancher.k8s.binbash.org/floatingip-subnet": "1.2.3.4/24",
	})

	err := r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	if assert.NotNil(t, created, "the ServiceGroup must be rebuilt from the persisted subnet") {
		assert.Equal(t, "1.2.3.4/24", created.Spec.Local.V4Pools[0].Subnet)
		assert.Equal(t, "1.2.3.4/32", created.Spec.Local.V4Pools[0].Pool)
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// TestController_reconcile_purelb_ipv6_pool pins the address family split:
// an IPv6 allocation belongs in V6Pools with a single-host /128, never in
// V4Pools with a /32.
func TestController_reconcile_purelb_ipv6_pool(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	var created *purelb.ServiceGroup
	mockPureLB := &MockPureLBClient{
		GetServiceGroupsFunc: func(ctx context.Context, namespace string) ([]purelb.ServiceGroup, error) {
			return nil, nil
		},
		CreateServiceGroupFunc: func(ctx context.Context, group *purelb.ServiceGroup) error {
			created = group
			return nil
		},
	}

	r, _, svc := newPureLBAdoptionReconciler(t, mockPureLB, recorder, map[string]string{
		"rancher.k8s.binbash.org/floatingip":        "2001:db8::1",
		"rancher.k8s.binbash.org/floatingip-subnet": "2001:db8::/64",
	})

	err := r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	if assert.NotNil(t, created, "the ServiceGroup must be rebuilt from the persisted subnet") {
		assert.Empty(t, created.Spec.Local.V4Pools, "an IPv6 address must not be placed in V4Pools")
		if assert.Len(t, created.Spec.Local.V6Pools, 1) {
			assert.Equal(t, "2001:db8::1/128", created.Spec.Local.V6Pools[0].Pool)
			assert.Equal(t, "2001:db8::/64", created.Spec.Local.V6Pools[0].Subnet)
			assert.Equal(t, "default", created.Spec.Local.V6Pools[0].Aggregation)
		}
	}
}

func TestPoolNameForAddress(t *testing.T) {
	tests := []struct {
		address string
		want    string
	}{
		{"1.2.3.4", "ip4-1.2.3.4"},
		{"2001:db8:0:0:0:0:0:1", "ip6-2001.db8.0.0.0.0.0.1"},
		{"2001:db8::1", "ip6-2001-db8--1"},
		{"::1", "ip6-1"},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			name := poolNameForAddress(tt.address)
			assert.Equal(t, tt.want, name)
			assert.Empty(t, validation.IsDNS1123Subdomain(name), "pool names must be valid Kubernetes object names")
		})
	}
}

func TestPoolContainsAddress(t *testing.T) {
	tests := []struct {
		name      string
		addresses []string
		ip        string
		want      bool
	}{
		{name: "ipv4 single host", addresses: []string{"1.2.3.4/32"}, ip: "1.2.3.4", want: true},
		{name: "ipv4 different host", addresses: []string{"1.2.3.4/32"}, ip: "1.2.3.5", want: false},
		{name: "ipv6 single host", addresses: []string{"2001:db8::1/128"}, ip: "2001:db8::1", want: true},
		{name: "ipv6 different host", addresses: []string{"2001:db8::1/128"}, ip: "2001:db8::2", want: false},
		{name: "legacy ipv6 /32 pool", addresses: []string{"2001:db8::1/32"}, ip: "2001:db8::1", want: true},
		{name: "family mismatch", addresses: []string{"1.2.3.4/32"}, ip: "2001:db8::1", want: false},
		{name: "bare address", addresses: []string{"2001:db8::1"}, ip: "2001:db8::1", want: true},
		{name: "unparsable entry", addresses: []string{"not-an-address"}, ip: "2001:db8::1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, poolContainsAddress(tt.addresses, net.ParseIP(tt.ip)))
		})
	}
}

// TestController_reconcile_skips_purelb_rebuild_without_subnet pins the guard:
// adopting an allocation whose subnet was never persisted must not create a
// ServiceGroup with an empty subnet.
func TestController_reconcile_skips_purelb_rebuild_without_subnet(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	createCalled := false
	mockPureLB := &MockPureLBClient{
		GetServiceGroupsFunc: func(ctx context.Context, namespace string) ([]purelb.ServiceGroup, error) {
			return nil, nil
		},
		CreateServiceGroupFunc: func(ctx context.Context, group *purelb.ServiceGroup) error {
			createCalled = true
			return nil
		},
	}

	r, clientset, svc := newPureLBAdoptionReconciler(t, mockPureLB, recorder, map[string]string{
		"rancher.k8s.binbash.org/floatingip": "1.2.3.4",
	})

	err := r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, createCalled, "a ServiceGroup must not be created with an unknown subnet")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "MissingServiceGroupSubnet")
	default:
		t.Error("expected a warning event about the missing ServiceGroup subnet")
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// newMetalLBReconciler builds a reconciler against a fake clientset with the
// app namespace, a project namespace, the project config Secret and the
// network-interface mapping the MetalLB reconcile path needs.
func newMetalLBReconciler(t *testing.T, mockIPAM *MockIPAMClient, mockMetalLB *MockMetalLBClient) (*reconciler, *fake.Clientset) {
	t.Helper()

	clientset := fake.NewSimpleClientset()
	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           record.NewFakeRecorder(10),
		appNamespace:       "rancher-fip-manager",
	}

	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	if _, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create app namespace: %v", err)
	}
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	if _, err := clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test namespace: %v", err)
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	if _, err := clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	if _, err := clientset.CoreV1().ConfigMaps("rancher-fip-manager").Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create configmap: %v", err)
	}

	return r, clientset
}

// TestController_reconcile_delete_keeps_address_used_by_other_service pins the
// shared-pool guard: the deleting Service no longer carries the
// floatingip-group annotation, but another Service still uses the address.
// Deleting or releasing from here would break that Service.
func TestController_reconcile_delete_keeps_address_used_by_other_service(t *testing.T) {
	released := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
	}
	poolDeleted, adDeleted := false, false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	otherSvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "other-svc",
			Namespace:   "test-ns",
			Annotations: map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), otherSvc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create other service: %v", err)
	}

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	if err := r.reconcile(context.Background(), svc); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	assert.True(t, released, "the deleting Service's own IPAM claim must be released even while another Service uses the address")
	assert.False(t, poolDeleted, "IPAddressPool must not be deleted while another Service uses the address")
	assert.False(t, adDeleted, "L2Advertisement must not be deleted while another Service uses the address")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, "rancher.k8s.binbash.org/floatingip")
}

// TestController_reconcile_delete_keeps_pool_selected_by_pool_allocation pins
// the pool-selector case for a peer that lost its floatingip annotation: the
// shared pool still selects that peer by label and namespace, so deleting the
// pool would break it. The deleting Service no longer carries the
// floatingip-group annotation, and its own IPAM claim must still be released.
func TestController_reconcile_delete_keeps_pool_selected_by_pool_allocation(t *testing.T) {
	var released []string
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = append(released, fmt.Sprintf("%s|%s|%s", serviceName, ipaddr, floatingIPGroup))
			return nil
		},
	}
	poolDeleted, adDeleted := false, false
	mockMetalLB := &MockMetalLBClient{
		GetIPAddressPoolFunc: func(ctx context.Context, name, namespace string) (*v1beta1.IPAddressPool, error) {
			return &v1beta1.IPAddressPool{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: v1beta1.IPAddressPoolSpec{
					Addresses: []string{"1.2.3.4/32"},
					AllocateTo: &v1beta1.ServiceAllocation{
						Namespaces: []string{"test-ns", "other-ns"},
						ServiceSelectors: []metav1.LabelSelector{{
							MatchLabels: map[string]string{"rancher.k8s.binbash.org/servicegroup": "shared-group"},
						}},
					},
				},
			}, nil
		},
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	if _, err := clientset.CoreV1().Namespaces().Create(context.Background(),
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-ns"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create other namespace: %v", err)
	}
	// The peer joined the group's pool but has no floatingip annotation.
	peer := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "peer-svc",
			Namespace: "other-ns",
			Labels:    map[string]string{"rancher.k8s.binbash.org/servicegroup": "shared-group"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("other-ns").Create(context.Background(), peer, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create peer service: %v", err)
	}

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	if err := r.reconcile(context.Background(), svc); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	assert.Equal(t, []string{"test-svc|1.2.3.4|"}, released,
		"the deleting Service's own claim must be released without relying on its group annotation")
	assert.False(t, poolDeleted, "IPAddressPool must not be deleted while its AllocateTo rules still select another Service")
	assert.False(t, adDeleted, "L2Advertisement must not be deleted while its pool is still shared")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, "rancher.k8s.binbash.org/floatingip")
}

// TestController_reconcile_delete_keeps_pool_claimed_in_ipam pins the second
// ownership source: the IPAM's claimant labels. A live Service listed for the
// address keeps the pool even when neither Service carries allocation
// annotations.
func TestController_reconcile_delete_keeps_pool_claimed_in_ipam(t *testing.T) {
	released := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{
				{Cluster: "c-12345", IPAddress: "1.2.3.4", ServiceNamespace: "test-ns", ServiceName: "test-svc"},
				{Cluster: "c-12345", IPAddress: "1.2.3.4", ServiceNamespace: "other-ns", ServiceName: "peer-svc"},
			}}, nil
		},
	}
	poolDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	if _, err := clientset.CoreV1().Namespaces().Create(context.Background(),
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-ns"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create other namespace: %v", err)
	}
	peer := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-svc", Namespace: "other-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("other-ns").Create(context.Background(), peer, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create peer service: %v", err)
	}

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	if err := r.reconcile(context.Background(), svc); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	assert.True(t, released, "the deleting Service's own claim must be released")
	assert.False(t, poolDeleted, "the pool must be kept while the IPAM lists another live Service for the address")
}

// TestController_reconcile_delete_ignores_stale_ipam_claim pins the liveness
// filter: an IPAM label left behind by a Service that no longer exists must not
// keep the pool (and the address) allocated forever.
func TestController_reconcile_delete_ignores_stale_ipam_claim(t *testing.T) {
	released := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{
				{Cluster: "c-12345", IPAddress: "1.2.3.4", ServiceNamespace: "other-ns", ServiceName: "gone-svc"},
			}}, nil
		},
	}
	poolDeleted, adDeleted := false, false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	if err := r.reconcile(context.Background(), svc); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	assert.True(t, released, "the last live claimant must release the address")
	assert.True(t, poolDeleted, "a stale IPAM label must not keep the pool allocated")
	assert.True(t, adDeleted, "a stale IPAM label must not keep the advertisement")
}

// TestController_reconcile_delete_ignores_non_lb_member pins the inverse case:
// a group member converted to ClusterIP keeps the servicegroup label and its
// floatingip annotation, but no longer uses the address. Counting it would
// keep the pool (and the address's pool name) allocated forever.
func TestController_reconcile_delete_ignores_non_lb_member(t *testing.T) {
	released := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{
				{Cluster: "c-12345", IPAddress: "1.2.3.4", ServiceNamespace: "other-ns", ServiceName: "converted-svc"},
			}}, nil
		},
	}
	poolDeleted, adDeleted := false, false
	mockMetalLB := &MockMetalLBClient{
		GetIPAddressPoolFunc: func(ctx context.Context, name, namespace string) (*v1beta1.IPAddressPool, error) {
			return &v1beta1.IPAddressPool{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: v1beta1.IPAddressPoolSpec{
					Addresses: []string{"1.2.3.4/32"},
					AllocateTo: &v1beta1.ServiceAllocation{
						Namespaces: []string{"test-ns", "other-ns"},
						ServiceSelectors: []metav1.LabelSelector{{
							MatchLabels: map[string]string{"rancher.k8s.binbash.org/servicegroup": "shared-group"},
						}},
					},
				},
			}, nil
		},
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	if _, err := clientset.CoreV1().Namespaces().Create(context.Background(),
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-ns"}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create other namespace: %v", err)
	}
	// Converted to ClusterIP: stale group label and allocation annotation.
	converted := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "converted-svc",
			Namespace: "other-ns",
			Labels:    map[string]string{"rancher.k8s.binbash.org/servicegroup": "shared-group"},
			Annotations: map[string]string{
				"rancher.k8s.binbash.org/floatingip": "1.2.3.4",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeClusterIP},
	}
	if _, err := clientset.CoreV1().Services("other-ns").Create(context.Background(), converted, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create converted service: %v", err)
	}

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	if err := r.reconcile(context.Background(), svc); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	assert.True(t, released, "the last LoadBalancer claimant must release the address")
	assert.True(t, poolDeleted, "a Service converted to ClusterIP must not pin the shared pool")
	assert.True(t, adDeleted, "a Service converted to ClusterIP must not pin the advertisement")
}

// TestController_reconcile_delete_recovers_allocation_without_group_annotation
// pins the recovery path: a pending allocation is found by service identity,
// not by the editable floatingip-group annotation.
func TestController_reconcile_delete_recovers_allocation_without_group_annotation(t *testing.T) {
	var releasedIP string
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			releasedIP = ipaddr
			return nil
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{
				{Cluster: "c-12345", IPAddress: "1.2.3.4", ServiceNamespace: "test-ns", ServiceName: "test-svc", FloatingIPGroup: "shared-group"},
			}}, nil
		},
	}
	poolDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip-request": "p-12345"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	if err := r.reconcile(context.Background(), svc); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	assert.Equal(t, "1.2.3.4", releasedIP, "the pending allocation must be recovered without the group annotation")
	assert.True(t, poolDeleted, "the recovered allocation was the only claim, so its pool must be deleted")
}

// TestController_reconcile_overlap_cleanup_keeps_pool_of_other_service pins the
// guard on the CIDR-overlap cleanup: the overlapping pool may belong to
// another live Service, so it must be left alone and the FIP must not be
// released.
func TestController_reconcile_overlap_cleanup_keeps_pool_of_other_service(t *testing.T) {
	released := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			return "1.2.3.4", "1.2.3.4/24", "", nil
		},
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
	}
	poolDeleted, adDeleted := false, false
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc: func(ctx context.Context, pool *v1beta1.IPAddressPool) error {
			return fmt.Errorf("admission webhook denied the request: CIDR \"1.2.3.4/32\" in pool \"ip4-1.2.3.4\" overlaps with already defined CIDR \"1.2.3.4/32\"")
		},
		GetIPAddressPoolsFunc: func(ctx context.Context, namespace string) ([]v1beta1.IPAddressPool, error) {
			return []v1beta1.IPAddressPool{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "ip4-1.2.3.4", Namespace: namespace},
					Spec:       v1beta1.IPAddressPoolSpec{Addresses: []string{"1.2.3.4/32"}},
				},
			}, nil
		},
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	otherSvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "other-svc",
			Namespace:   "test-ns",
			Annotations: map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), otherSvc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create other service: %v", err)
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	if _, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	err := r.reconcile(context.Background(), svc)
	assert.Error(t, err, "reconcile must report the unresolved overlap")
	assert.False(t, poolDeleted, "the overlapping pool of another Service must not be deleted")
	assert.False(t, adDeleted, "the advertisement of another Service must not be deleted")
	assert.False(t, released, "the FIP must not be released while another Service uses the address")
}

func TestController_updateFunc(t *testing.T) {
	base := func() *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", ResourceVersion: "1"},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		}
	}

	tests := []struct {
		name     string
		mutate   func(oldSvc, newSvc *v1.Service)
		expected bool
	}{
		{
			name:     "resync of an unchanged LoadBalancer",
			mutate:   func(oldSvc, newSvc *v1.Service) {},
			expected: true,
		},
		{
			name: "ClusterIP switched to LoadBalancer",
			mutate: func(oldSvc, newSvc *v1.Service) {
				oldSvc.Spec.Type = v1.ServiceTypeClusterIP
			},
			expected: true,
		},
		{
			name: "LoadBalancer switched to ClusterIP with an allocation",
			mutate: func(oldSvc, newSvc *v1.Service) {
				newSvc.ResourceVersion = "2"
				newSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.Annotations = map[string]string{floatingIPAnnotation: "1.2.3.4"}
			},
			expected: true,
		},
		{
			name: "ClusterIP resync with an allocation",
			mutate: func(oldSvc, newSvc *v1.Service) {
				oldSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.Annotations = map[string]string{floatingIPAnnotation: "1.2.3.4"}
			},
			expected: true,
		},
		{
			name: "static-ip annotation added",
			mutate: func(oldSvc, newSvc *v1.Service) {
				newSvc.ResourceVersion = "2"
				newSvc.Annotations = map[string]string{"rancher.k8s.binbash.org/static-ip": "1.2.3.4"}
			},
			expected: true,
		},
		{
			name: "finalizer removed by hand",
			mutate: func(oldSvc, newSvc *v1.Service) {
				newSvc.ResourceVersion = "2"
				oldSvc.Finalizers = []string{finalizerName}
			},
			expected: true,
		},
		{
			name: "unrelated update",
			mutate: func(oldSvc, newSvc *v1.Service) {
				newSvc.ResourceVersion = "2"
				newSvc.Labels = map[string]string{"foo": "bar"}
			},
			expected: false,
		},
		{
			name: "ClusterIP update",
			mutate: func(oldSvc, newSvc *v1.Service) {
				oldSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.ResourceVersion = "2"
			},
			expected: false,
		},
		{
			name: "deletion of a non-LoadBalancer Service",
			mutate: func(oldSvc, newSvc *v1.Service) {
				now := metav1.Now()
				oldSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.Spec.Type = v1.ServiceTypeClusterIP
				newSvc.ResourceVersion = "2"
				newSvc.DeletionTimestamp = &now
				newSvc.Finalizers = []string{finalizerName}
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSvc, newSvc := base(), base()
			tt.mutate(oldSvc, newSvc)

			c := &Controller{queue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())}
			c.updateFunc(oldSvc, newSvc)

			assert.Equal(t, tt.expected, c.queue.Len() > 0)
			if tt.expected {
				key, _ := c.queue.Get()
				assert.Equal(t, "ns/svc", key)
			}
		})
	}
}

func TestController_processNextItem_dropsInvalidKey(t *testing.T) {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer queue.ShutDown()

	c := &Controller{queue: queue}
	queue.Add("ns/name/extra")

	assert.True(t, c.processNextItem(context.Background()))
	assert.Equal(t, 0, queue.Len(), "a malformed key can never reconcile; it must be forgotten instead of requeued")
}

func TestController_deleteFunc_unwrapsTombstone(t *testing.T) {
	c := &Controller{
		queue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		tombstones: make(map[string]*v1.Service),
	}
	defer c.queue.ShutDown()

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-svc",
			Namespace:  "test-ns",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				floatingIPAnnotation: "1.2.3.4",
			},
		},
	}

	c.deleteFunc(cache.DeletedFinalStateUnknown{Key: "test-ns/test-svc", Obj: svc})

	assert.Equal(t, 1, c.queue.Len(), "a tombstoned delete must still be enqueued")
	key, _ := c.queue.Get()
	assert.Equal(t, "test-ns/test-svc", key)
	c.queue.Done(key)

	snapshot := c.takeTombstone("test-ns/test-svc")
	if assert.NotNil(t, snapshot, "the last known state must be kept for the queued key") {
		assert.Equal(t, svc, snapshot)
	}
	assert.Nil(t, c.takeTombstone("test-ns/test-svc"), "the snapshot is consumed once")
}

func TestController_deleteFunc_ignoresTombstoneWithoutAllocation(t *testing.T) {
	c := &Controller{
		queue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		tombstones: make(map[string]*v1.Service),
	}
	defer c.queue.ShutDown()

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
	}

	c.deleteFunc(cache.DeletedFinalStateUnknown{Key: "test-ns/test-svc", Obj: svc})

	assert.Nil(t, c.takeTombstone("test-ns/test-svc"), "a Service without finalizer or allocation needs no snapshot")
}

func TestController_reconcile_cleansUpTombstonedService(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(10)

	releasedIP := ""
	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", fmt.Errorf("unexpected FIP request for a deleted Service")
		},
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			releasedIP = ipaddr
			return nil
		},
	}
	poolDeleted := false
	adDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}

	informer := informers.NewSharedInformerFactory(clientset, 0).Core().V1().Services().Informer()
	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}
	c := &Controller{
		serviceInformer: informer,
		queue:           workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		reconciler:      r,
		tombstones:      make(map[string]*v1.Service),
	}
	defer c.queue.ShutDown()

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err := clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.com/other"},
			Annotations: map[string]string{
				floatingIPAnnotation: "1.2.3.4",
				projectAnnotation:    "p-12345",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	// The informer was disconnected when the finalizer was removed and the
	// Service deleted, so its delete event arrives as a tombstone and the
	// object is no longer in the cache.
	c.deleteFunc(cache.DeletedFinalStateUnknown{Key: "test-ns/test-svc", Obj: svc})

	err = c.reconcile(context.Background(), "test-ns/test-svc")
	assert.NoError(t, err)
	assert.False(t, requestCalled, "a deleted Service must never request a new FloatingIP")
	assert.Equal(t, "1.2.3.4", releasedIP, "the snapshot must be used to release the allocation")
	assert.True(t, poolDeleted, "the IPAddressPool of the deleted Service must be removed")
	assert.True(t, adDeleted, "the L2Advertisement of the deleted Service must be removed")
	assert.Nil(t, c.takeTombstone("test-ns/test-svc"))
}

func TestController_reconcile_retriesTombstonedServiceAfterError(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(10)

	releaseAttempts := 0
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			releaseAttempts++
			if releaseAttempts == 1 {
				return fmt.Errorf("transient IPAM failure")
			}
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc:   func(ctx context.Context, name, namespace string) error { return nil },
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error { return nil },
	}

	informer := informers.NewSharedInformerFactory(clientset, 0).Core().V1().Services().Informer()
	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logrus.New(),
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}
	c := &Controller{
		serviceInformer: informer,
		queue:           workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		reconciler:      r,
		tombstones:      make(map[string]*v1.Service),
	}
	defer c.queue.ShutDown()

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err := clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.com/other"},
			Annotations: map[string]string{
				floatingIPAnnotation: "1.2.3.4",
				projectAnnotation:    "p-12345",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	c.deleteFunc(cache.DeletedFinalStateUnknown{Key: "test-ns/test-svc", Obj: svc})

	err = c.reconcile(context.Background(), "test-ns/test-svc")
	assert.Error(t, err, "the transient IPAM failure must be returned so the queue retries")
	assert.NotNil(t, c.peekTombstone("test-ns/test-svc"), "the snapshot must survive a failed reconcile")

	err = c.reconcile(context.Background(), "test-ns/test-svc")
	assert.NoError(t, err)
	assert.Equal(t, 2, releaseAttempts, "the retry must release the address from the retained snapshot")
	assert.Nil(t, c.peekTombstone("test-ns/test-svc"), "the snapshot is dropped once the cleanup succeeds")
}

func TestController_reconcile_recovers_missing_floatingip_annotation(t *testing.T) {
	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", fmt.Errorf("unexpected FIP request")
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{{
				Cluster:          "c-12345",
				ServiceNamespace: "test-ns",
				ServiceName:      "test-svc",
				IPAddress:        "1.2.3.4",
			}}}, nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc:   func(ctx context.Context, pool *v1beta1.IPAddressPool) error { return nil },
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error { return nil },
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			},
		},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requestCalled, "a Service with an ingress must not request a second allocation")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "1.2.3.4", updatedSvc.Annotations["rancher.k8s.binbash.org/floatingip"])
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// A Service that carries the pending marker but no floatingip annotation was
// interrupted between RequestFIP and the annotation write. A restart must adopt
// the allocation from the IPAM instead of requesting a second address.
func TestController_reconcile_adopts_pending_allocation(t *testing.T) {
	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", fmt.Errorf("unexpected FIP request")
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{{
				Cluster:          "c-12345",
				ServiceNamespace: "test-ns",
				ServiceName:      "test-svc",
				IPAddress:        "1.2.3.4",
			}}}, nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc:   func(ctx context.Context, pool *v1beta1.IPAddressPool) error { return nil },
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error { return nil },
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-svc",
			Namespace:   "test-ns",
			Annotations: map[string]string{floatingIPRequestAnnotation: "p-12345"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requestCalled, "a pending allocation must be adopted, not requested again")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "1.2.3.4", updatedSvc.Annotations[floatingIPAnnotation])
	assert.NotContains(t, updatedSvc.Annotations, floatingIPRequestAnnotation)
	assert.Equal(t, "p-12345", updatedSvc.Annotations[projectAnnotation], "the marker's project must survive adoption")
	assert.Contains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// A pending allocation whose floatingip-group annotation changed after the
// request must still be adopted by the Service labels alone. Re-requesting
// would allocate a second address.
func TestController_reconcile_adopts_pending_allocation_after_group_change(t *testing.T) {
	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", fmt.Errorf("unexpected FIP request")
		},
		ListFIPsFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error) {
			return &ipam.FIPListResponse{FloatingIPs: []ipam.FloatingIP{{
				Cluster:          "c-12345",
				ServiceNamespace: "test-ns",
				ServiceName:      "test-svc",
				IPAddress:        "1.2.3.4",
				FloatingIPGroup:  "old-group",
			}}}, nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc:   func(ctx context.Context, pool *v1beta1.IPAddressPool) error { return nil },
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error { return nil },
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "test-ns",
			Annotations: map[string]string{
				floatingIPRequestAnnotation:                "p-12345",
				"rancher.k8s.binbash.org/floatingip-group": "new-group",
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requestCalled, "the existing allocation must be adopted regardless of the group annotation")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "1.2.3.4", updatedSvc.Annotations[floatingIPAnnotation])
}

// A Service deleted between the informer read and the pending-marker write must
// not be allocated an address that no Service can release.
func TestController_reconcile_deleted_before_request_does_not_allocate(t *testing.T) {
	requested := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requested = true
			return "1.2.3.4", "1.2.3.4/24", "", nil
		},
	}
	r, _ := newMetalLBReconciler(t, mockIPAM, &MockMetalLBClient{})

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "gone-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	err := r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requested, "a Service that no longer exists must not get an allocation")
}

// A Service deleted after RequestFIP but before the finalizer/annotation write
// must have its allocation released from the cached object.
func TestController_reconcile_deleted_after_request_releases_allocation(t *testing.T) {
	var clientset *fake.Clientset
	released := ""
	poolDeleted, adDeleted := false, false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			if err := clientset.CoreV1().Services("test-ns").Delete(ctx, "test-svc", metav1.DeleteOptions{}); err != nil {
				t.Errorf("failed to delete service during request: %v", err)
			}
			return "1.2.3.4", "1.2.3.4/24", "", nil
		},
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = ipaddr
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc:   func(ctx context.Context, pool *v1beta1.IPAddressPool) error { return nil },
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error { return nil },
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	var r *reconciler
	r, clientset = newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.Equal(t, "1.2.3.4", released, "the vanished Service's allocation must be released")
	assert.True(t, poolDeleted, "the pool created for the vanished Service must be deleted")
	assert.True(t, adDeleted, "the advertisement created for the vanished Service must be deleted")
}

// A Service converted from LoadBalancer to ClusterIP must release its
// allocation instead of keeping the address and finalizer forever.
func TestController_reconcile_clusterip_releases_allocation(t *testing.T) {
	released := false
	poolDeleted, adDeleted := false, false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-svc",
			Namespace:   "test-ns",
			Finalizers:  []string{finalizerName},
			Annotations: map[string]string{floatingIPAnnotation: "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeClusterIP},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.True(t, released, "a ClusterIP conversion must release the address")
	assert.True(t, poolDeleted, "the pool of the converted Service must be deleted")
	assert.True(t, adDeleted, "the advertisement of the converted Service must be deleted")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, floatingIPAnnotation)
}

// A plain ClusterIP Service must never be allocated a FloatingIP, even if it
// reaches reconcile through a stale queue entry.
func TestController_reconcile_plain_clusterip_does_nothing(t *testing.T) {
	requested := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requested = true
			return "1.2.3.4", "1.2.3.4/24", "", nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, &MockMetalLBClient{})

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeClusterIP},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requested, "a ClusterIP Service must not request a FloatingIP")
}

func TestController_reconcile_delete_resumes_after_resources_deleted(t *testing.T) {
	released := false
	poolDeleted, adDeleted := false, false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				floatingIPAnnotation:      "1.2.3.4",
				cleanupProgressAnnotation: cleanupStageResourcesDeleted,
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.True(t, released, "the release must resume after the resources were deleted")
	assert.False(t, poolDeleted, "an already-deleted pool must not be deleted again")
	assert.False(t, adDeleted, "an already-deleted advertisement must not be deleted again")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, cleanupProgressAnnotation)
}

func TestController_reconcile_delete_resumes_after_release(t *testing.T) {
	released := false
	poolDeleted, adDeleted := false, false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				floatingIPAnnotation:      "1.2.3.4",
				cleanupProgressAnnotation: cleanupStageReleased,
			},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, released, "an already-released allocation must not be released again")
	assert.False(t, poolDeleted, "an already-deleted pool must not be deleted again")
	assert.False(t, adDeleted, "an already-deleted advertisement must not be deleted again")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
	assert.NotContains(t, updatedSvc.Annotations, cleanupProgressAnnotation)
}

func TestController_reconcile_leaves_foreign_ingress_alone(t *testing.T) {
	requestCalled := false
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			requestCalled = true
			return "", "", "", fmt.Errorf("unexpected FIP request")
		},
	}
	mockMetalLB := &MockMetalLBClient{}
	r, clientset := newMetalLBReconciler(t, mockIPAM, mockMetalLB)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "5.6.7.8"}},
			},
		},
	}
	_, err := clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.False(t, requestCalled, "a Service with an address the IPAM does not know must not request one")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.Annotations, "rancher.k8s.binbash.org/floatingip")
	assert.NotContains(t, updatedSvc.ObjectMeta.Finalizers, finalizerName)
}

// A finalizer removed by hand while another finalizer keeps the Service alive
// must still release the allocation and remove its pool: otherwise the object
// eventually disappears and both leak.
func TestController_reconcile_delete_after_finalizer_removed_by_hand(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	logger := logrus.New()
	recorder := record.NewFakeRecorder(10)

	released := false
	mockIPAM := &MockIPAMClient{
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			released = true
			return nil
		},
	}
	poolDeleted := false
	adDeleted := false
	mockMetalLB := &MockMetalLBClient{
		DeleteIPAddressPoolFunc: func(ctx context.Context, name, namespace string) error {
			poolDeleted = true
			return nil
		},
		DeleteL2AdvertisementFunc: func(ctx context.Context, name, namespace string) error {
			adDeleted = true
			return nil
		},
	}

	r := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		ipamClientOverride: mockIPAM,
		logger:             logger,
		recorder:           recorder,
		appNamespace:       "rancher-fip-manager",
	}

	appNs := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), appNs, metav1.CreateOptions{})
	assert.NoError(t, err)

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	assert.NoError(t, err)

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	_, err = clientset.CoreV1().Secrets("rancher-fip-manager").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	now := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-svc",
			Namespace:         "test-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.com/other"},
			Annotations:       map[string]string{"rancher.k8s.binbash.org/floatingip": "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	_, err = clientset.CoreV1().Services("test-ns").Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = r.reconcile(context.Background(), svc)
	assert.NoError(t, err)
	assert.True(t, released, "the allocation must be released without the finalizer")
	assert.True(t, poolDeleted, "the IPAddressPool must be deleted without the finalizer")
	assert.True(t, adDeleted, "the L2Advertisement must be deleted without the finalizer")

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, updatedSvc.Annotations, "rancher.k8s.binbash.org/floatingip")
}

func TestReconciler_ipamClientFor_cachesPerProjectAndRebuildsOnSecretChange(t *testing.T) {
	var built []string
	r := &reconciler{
		newIPAMClient: func(apiURL, clientID string, caStore *ipam.CAStore) (ipamClient, error) {
			built = append(built, apiURL+"|"+clientID)
			return &MockIPAMClient{}, nil
		},
	}

	secretA := &v1.Secret{Data: map[string][]byte{
		"apiUrl":  []byte("https://ipam-a"),
		"cluster": []byte("c-a"),
	}}
	secretB := &v1.Secret{Data: map[string][]byte{
		"apiUrl":  []byte("https://ipam-b"),
		"cluster": []byte("c-b"),
	}}

	a1, err := r.ipamClientFor("p-a", secretA)
	assert.NoError(t, err)
	a2, err := r.ipamClientFor("p-a", secretA)
	assert.NoError(t, err)
	assert.True(t, a1 == a2, "the same project and Secret must reuse the cached client")

	b1, err := r.ipamClientFor("p-b", secretB)
	assert.NoError(t, err)
	assert.False(t, a1 == b1, "different projects must not share a client")

	changedA := secretA.DeepCopy()
	changedA.Data["apiUrl"] = []byte("https://ipam-a2")
	a3, err := r.ipamClientFor("p-a", changedA)
	assert.NoError(t, err)
	assert.False(t, a1 == a3, "a changed apiUrl must rebuild the client")

	assert.Equal(t, []string{
		"https://ipam-a|client-c-a",
		"https://ipam-b|client-c-b",
		"https://ipam-a2|client-c-a",
	}, built)
}

func TestReconciler_ipamClientFor_concurrentAccess(t *testing.T) {
	var builds int32
	r := &reconciler{
		newIPAMClient: func(apiURL, clientID string, caStore *ipam.CAStore) (ipamClient, error) {
			atomic.AddInt32(&builds, 1)
			return &MockIPAMClient{}, nil
		},
	}

	projects := []string{"p-a", "p-b", "p-c", "p-d"}
	secrets := make(map[string]*v1.Secret, len(projects))
	for _, project := range projects {
		secrets[project] = &v1.Secret{Data: map[string][]byte{
			"apiUrl":  []byte("https://ipam-" + project),
			"cluster": []byte("c-" + project),
		}}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := 0; i < 32; i++ {
		for _, project := range projects {
			wg.Add(1)
			go func(project string) {
				defer wg.Done()
				if _, err := r.ipamClientFor(project, secrets[project]); err != nil {
					errs <- err
				}
			}(project)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("ipamClientFor returned an error: %v", err)
	}

	assert.Equal(t, int32(len(projects)), atomic.LoadInt32(&builds),
		"concurrent reconciles must build exactly one client per project")
}

func TestNew_indexesServicesByFloatingIP(t *testing.T) {
	c := New(fake.NewSimpleClientset(), nil, nil, "rancher-fip-manager", nil)

	_, ok := c.serviceInformer.GetIndexer().GetIndexers()[floatingIPIndex]
	assert.True(t, ok, "the Service informer must index by the floatingip annotation")
}

func TestReconciler_updateService_retriesConflict(t *testing.T) {
	clientset := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
	})

	conflicts := 0
	clientset.PrependReactor("update", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if conflicts < 2 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "services"}, "svc", fmt.Errorf("conflict"))
		}
		return false, nil, nil
	})

	r := &reconciler{clientset: clientset}
	updated, err := r.updateService(context.Background(), "ns", "svc", func(svc *v1.Service) bool {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations["test"] = "value"
		return true
	})

	assert.NoError(t, err)
	assert.NotNil(t, updated)
	assert.Equal(t, 2, conflicts, "the update must be retried through conflicts")

	stored, err := clientset.CoreV1().Services("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "value", stored.Annotations["test"])
}

func TestReconciler_updateService_boundsConflictRetries(t *testing.T) {
	clientset := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
	})

	attempts := 0
	clientset.PrependReactor("update", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "services"}, "svc", fmt.Errorf("conflict"))
	})

	r := &reconciler{clientset: clientset}
	_, err := r.updateService(context.Background(), "ns", "svc", func(*v1.Service) bool { return true })

	assert.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
	assert.Equal(t, serviceUpdateBackoff.Steps, attempts,
		"a reconcile must not retry conflicts without bound")
}

func TestReconciler_addressInUse_usesInformerIndex(t *testing.T) {
	peer := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "peer",
			Namespace:   "other-ns",
			Annotations: map[string]string{floatingIPAnnotation: "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	self := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "self",
			Namespace:   "ns",
			Annotations: map[string]string{floatingIPAnnotation: "1.2.3.4"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		floatingIPIndex: indexServiceByFloatingIP,
	})
	assert.NoError(t, indexer.Add(peer))
	assert.NoError(t, indexer.Add(self))

	clientset := fake.NewSimpleClientset(peer, self)
	clientset.PrependReactor("list", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("addressInUse must not list all services")
	})

	r := &reconciler{clientset: clientset, serviceIndexer: indexer}
	inUse, err := r.addressInUse(context.Background(), self, "1.2.3.4")
	assert.NoError(t, err)
	assert.True(t, inUse, "another Service referencing the address must be found")

	inUse, err = r.addressInUse(context.Background(), self, "5.6.7.8")
	assert.NoError(t, err)
	assert.False(t, inUse, "an address no Service references must not be reported in use")
}

func TestReconciler_addressInUse_ignoresStaleIndexEntry(t *testing.T) {
	// The index still records the peer as the address owner, but the peer's
	// annotation was already removed in the API (a just-finished deletion).
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		floatingIPIndex: indexServiceByFloatingIP,
	})
	assert.NoError(t, indexer.Add(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "peer",
			Namespace:   "other-ns",
			Annotations: map[string]string{floatingIPAnnotation: "1.2.3.4"},
		},
	}))
	livePeer := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "peer", Namespace: "other-ns"}}

	r := &reconciler{clientset: fake.NewSimpleClientset(livePeer), serviceIndexer: indexer}
	self := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "self", Namespace: "ns"}}

	inUse, err := r.addressInUse(context.Background(), self, "1.2.3.4")
	assert.NoError(t, err)
	assert.False(t, inUse, "a stale cache entry must not keep a released address alive")
}

// TestControllerRunDrainsInFlightReconcile verifies that cancelling the run
// context stops the queue without abandoning a reconcile in progress: Run must
// wait for the in-flight allocation to finish instead of returning (and letting
// main exit) between RequestFIP and the annotation write.
func TestControllerRunDrainsInFlightReconcile(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "test-ns",
			Labels: map[string]string{"field.cattle.io/projectId": "p-12345"},
		}},
		&v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
			Data: map[string][]byte{
				"apiUrl":           []byte("http://localhost"),
				"clientSecret":     []byte("secret"),
				"floatingIPPool":   []byte("pool1"),
				"cluster":          []byte("c-12345"),
				"project":          []byte("p-12345"),
				"loadBalancerType": []byte("metallb"),
			},
		},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
			Data:       map[string]string{"pool1": "eth0"},
		},
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		},
	)

	requestStarted := make(chan struct{})
	finishRequest := make(chan struct{})
	var startOnce sync.Once
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			startOnce.Do(func() { close(requestStarted) })
			<-finishRequest
			return "1.2.3.4", "1.2.3.4/24", "", nil
		},
	}
	mockMetalLB := &MockMetalLBClient{
		CreateIPAddressPoolFunc:   func(ctx context.Context, pool *v1beta1.IPAddressPool) error { return nil },
		CreateL2AdvertisementFunc: func(ctx context.Context, ad *v1beta1.L2Advertisement) error { return nil },
	}

	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	serviceInformer := informerFactory.Core().V1().Services().Informer()
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	reconciler := &reconciler{
		clientset:          clientset,
		metallbClient:      mockMetalLB,
		purelbClient:       &MockPureLBClient{},
		queue:              queue,
		recorder:           record.NewFakeRecorder(10),
		appNamespace:       "rancher-fip-manager",
		serviceIndexer:     serviceInformer.GetIndexer(),
		ipamClientOverride: mockIPAM,
	}
	c := &Controller{
		clientset:       clientset,
		serviceInformer: serviceInformer,
		queue:           queue,
		reconciler:      reconciler,
	}
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: c.addFunc})

	ctx, cancel := context.WithCancel(context.Background())
	runReturned := make(chan struct{})
	go func() {
		c.Run(ctx, 1)
		close(runReturned)
	}()

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("reconcile never reached the IPAM request")
	}

	cancel()

	select {
	case <-runReturned:
		t.Fatal("Run returned while a reconcile was still in flight; the allocation would be abandoned")
	case <-time.After(200 * time.Millisecond):
	}

	close(finishRequest)

	select {
	case <-runReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the in-flight reconcile finished")
	}

	updatedSvc, err := clientset.CoreV1().Services("test-ns").Get(context.Background(), "test-svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "1.2.3.4", updatedSvc.Annotations[floatingIPAnnotation], "the drained reconcile must persist its allocation")
}
