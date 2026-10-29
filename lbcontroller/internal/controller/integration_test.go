package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeyloman/rancher-fip-lb-controller/pkg/metallb"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/purelb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.universe.tf/metallb/api/v1beta1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// envtestBinaryPath resolves an envtest control-plane binary the same way
// envtest does: TEST_ASSET_<NAME>, then KUBEBUILDER_ASSETS, then the default
// install directory.
func envtestBinaryPath(binary string) string {
	envVar := "TEST_ASSET_" + strings.ToUpper(strings.ReplaceAll(binary, "-", "_"))
	if path, ok := os.LookupEnv(envVar); ok {
		return path
	}
	if dir, ok := os.LookupEnv("KUBEBUILDER_ASSETS"); ok {
		return filepath.Join(dir, binary)
	}
	return filepath.Join("/usr/local/kubebuilder/bin", binary)
}

func envtestBinariesAvailable() bool {
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(envtestBinaryPath(binary))
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return false
		}
	}
	return true
}

// startEnvtest starts a control plane for the test, skipping it when the
// envtest binaries are not installed.
func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()

	if !envtestBinariesAvailable() {
		t.Skip("envtest binaries not found; set KUBEBUILDER_ASSETS to run the integration tests")
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")},
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})

	return cfg
}

func TestController_Integration_MetalLB(t *testing.T) {
	cfg := startEnvtest(t)

	err := v1beta1.AddToScheme(scheme.Scheme)
	assert.NoError(t, err)

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	assert.NoError(t, err)
	assert.NotNil(t, k8sClient)

	clientset, err := kubernetes.NewForConfig(cfg)
	assert.NoError(t, err)

	metallbClient, err := metallb.NewClient(cfg)
	assert.NoError(t, err)

	// Check if MetalLB CRDs are available by trying to create a test IPAddressPool
	// This will fail immediately if CRDs aren't available
	testPool := &v1beta1.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-metallb-check",
			Namespace: "default",
		},
		Spec: v1beta1.IPAddressPoolSpec{
			Addresses: []string{"1.1.1.1/32"},
		},
	}
	err = metallbClient.CreateIPAddressPool(context.Background(), testPool)
	if err != nil {
		errStr := err.Error()
		// Check if the error is due to CRDs not being available
		if contains(errStr, "no matches for kind") || contains(errStr, "no matches for metallb.io") || contains(errStr, "unable to retrieve the complete list of server APIs") {
			t.Skip("MetalLB CRDs are not available in the test environment, skipping integration test")
		}
		// If it's AlreadyExists or another error, CRDs are available, clean up and continue
		if !apierrors.IsAlreadyExists(err) {
			// Try to clean up the test pool if it was created
			_ = metallbClient.DeleteIPAddressPool(context.Background(), "test-metallb-check", "default")
		}
	} else {
		// Clean up the test pool
		_ = metallbClient.DeleteIPAddressPool(context.Background(), "test-metallb-check", "default")
	}

	purelbClient, err := purelb.NewClient(cfg)
	assert.NoError(t, err)

	controller := New(clientset, metallbClient, purelbClient, "rancher-fip-manager", nil)

	// In the integration test, we still want to mock the IPAM client
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			if serviceName == "test-svc-ipv6" {
				return "2001:db8::1", "2001:db8::/64", "shared-key", nil
			}
			return "1.2.3.4", "1.2.3.4/24", "shared-key", nil
		},
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			return nil
		},
	}
	controller.reconciler.ipamClientOverride = mockIPAM

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.Run(ctx, 1)

	// Create a namespace
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	err = k8sClient.Create(context.Background(), ns)
	assert.NoError(t, err)

	// Create rancher-fip-manager namespace
	rancherNS := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	err = k8sClient.Create(context.Background(), rancherNS)
	assert.NoError(t, err)

	// Create a secret
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost:8080"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("metallb"),
		},
	}
	err = k8sClient.Create(context.Background(), secret)
	assert.NoError(t, err)

	// Create a configmap
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	err = k8sClient.Create(context.Background(), cm)
	assert.NoError(t, err)

	// Create a service
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "test-ns"},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{Port: 80, Protocol: v1.ProtocolTCP},
			},
		},
	}
	err = k8sClient.Create(context.Background(), svc)
	assert.NoError(t, err)

	// Wait for the IPAddressPool and L2Advertisement to be created
	assert.Eventually(t, func() bool {
		pool := &v1beta1.IPAddressPool{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, pool)
		return err == nil
	}, 10*time.Second, 1*time.Second)

	assert.Eventually(t, func() bool {
		ad := &v1beta1.L2Advertisement{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, ad)
		return err == nil
	}, 10*time.Second, 1*time.Second)

	// Delete the service
	err = k8sClient.Delete(context.Background(), svc)
	assert.NoError(t, err)

	// Wait for the IPAddressPool and L2Advertisement to be deleted
	assert.Eventually(t, func() bool {
		pool := &v1beta1.IPAddressPool{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, pool)
		return apierrors.IsNotFound(err)
	}, 10*time.Second, 1*time.Second)

	assert.Eventually(t, func() bool {
		ad := &v1beta1.L2Advertisement{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, ad)
		return apierrors.IsNotFound(err)
	}, 10*time.Second, 1*time.Second)

	// IPv6: the pool must contain a single /128, not the /32 the controller
	// used to emit for every address.
	ipv6PoolName := "ip6-2001-db8--1"
	ipv6Svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc-ipv6", Namespace: "test-ns"},
		Spec: v1.ServiceSpec{
			Type:  v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
		},
	}
	err = k8sClient.Create(context.Background(), ipv6Svc)
	assert.NoError(t, err)

	assert.Eventually(t, func() bool {
		pool := &v1beta1.IPAddressPool{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: ipv6PoolName, Namespace: "rancher-fip-manager"}, pool)
		return err == nil
	}, 10*time.Second, 1*time.Second)

	var ipv6Pool v1beta1.IPAddressPool
	err = k8sClient.Get(context.Background(), client.ObjectKey{Name: ipv6PoolName, Namespace: "rancher-fip-manager"}, &ipv6Pool)
	require.NoError(t, err)
	require.Equal(t, []string{"2001:db8::1/128"}, ipv6Pool.Spec.Addresses)

	assert.Eventually(t, func() bool {
		ad := &v1beta1.L2Advertisement{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: ipv6PoolName, Namespace: "rancher-fip-manager"}, ad)
		return err == nil
	}, 10*time.Second, 1*time.Second)

	err = k8sClient.Delete(context.Background(), ipv6Svc)
	assert.NoError(t, err)

	assert.Eventually(t, func() bool {
		pool := &v1beta1.IPAddressPool{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: ipv6PoolName, Namespace: "rancher-fip-manager"}, pool)
		return apierrors.IsNotFound(err)
	}, 10*time.Second, 1*time.Second)
}

func TestController_Integration_PureLB(t *testing.T) {
	cfg := startEnvtest(t)

	err := purelb.AddToScheme(scheme.Scheme)
	assert.NoError(t, err)

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	assert.NoError(t, err)
	assert.NotNil(t, k8sClient)

	clientset, err := kubernetes.NewForConfig(cfg)
	assert.NoError(t, err)

	purelbClient, err := purelb.NewClient(cfg)
	assert.NoError(t, err)

	// Check if PureLB CRDs are available by trying to create a test ServiceGroup
	// This will fail immediately if CRDs aren't available
	testServiceGroup := &purelb.ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-purelb-check",
			Namespace: "default",
		},
		Spec: purelb.ServiceGroupSpec{
			Local: &purelb.ServiceGroupLocal{
				V4Pools: []purelb.AddressPool{
					{
						Subnet: "1.1.1.0/24",
						Pool:   "1.1.1.1/32",
					},
				},
			},
		},
	}
	err = purelbClient.CreateServiceGroup(context.Background(), testServiceGroup)
	if err != nil {
		errStr := err.Error()
		// Check if the error is due to CRDs not being available
		if contains(errStr, "no matches for kind") || contains(errStr, "no matches for purelb.io") || contains(errStr, "unable to retrieve the complete list of server APIs") {
			t.Skip("PureLB CRDs are not available in the test environment, skipping integration test")
		}
		// If it's AlreadyExists or another error, CRDs are available, clean up and continue
		if !apierrors.IsAlreadyExists(err) {
			// Try to clean up the test ServiceGroup if it was created
			_ = purelbClient.DeleteServiceGroup(context.Background(), "test-purelb-check", "default")
		}
	} else {
		// Clean up the test ServiceGroup
		_ = purelbClient.DeleteServiceGroup(context.Background(), "test-purelb-check", "default")
	}

	metallbClient, err := metallb.NewClient(cfg)
	assert.NoError(t, err)

	controller := New(clientset, metallbClient, purelbClient, "rancher-fip-manager", nil)

	// In the integration test, we still want to mock the IPAM client
	mockIPAM := &MockIPAMClient{
		RequestFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
			return "1.2.3.4", "1.2.3.4/24", "shared-key", nil
		},
		ReleaseFIPFunc: func(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
			return nil
		},
	}
	controller.reconciler.ipamClientOverride = mockIPAM

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go controller.Run(ctx, 1)

	// Create a namespace
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns-purelb", Labels: map[string]string{"field.cattle.io/projectId": "p-12345"}}}
	err = k8sClient.Create(context.Background(), ns)
	assert.NoError(t, err)

	// Create rancher-fip-manager namespace
	rancherNS := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-manager"}}
	err = k8sClient.Create(context.Background(), rancherNS)
	assert.NoError(t, err)

	// Create a secret with PureLB loadBalancerType
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-12345", Namespace: "rancher-fip-manager"},
		Data: map[string][]byte{
			"apiUrl":           []byte("http://localhost:8080"),
			"clientId":         []byte("id"),
			"clientSecret":     []byte("secret"),
			"floatingIPPool":   []byte("pool1"),
			"cluster":          []byte("c-12345"),
			"project":          []byte("p-12345"),
			"loadBalancerType": []byte("purelb"),
		},
	}
	err = k8sClient.Create(context.Background(), secret)
	assert.NoError(t, err)

	// Create a configmap
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "network-interface-mappings", Namespace: "rancher-fip-manager"},
		Data:       map[string]string{"pool1": "eth0"},
	}
	err = k8sClient.Create(context.Background(), cm)
	assert.NoError(t, err)

	// Create a service
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc-purelb", Namespace: "test-ns-purelb"},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{Port: 80, Protocol: v1.ProtocolTCP},
			},
		},
	}
	err = k8sClient.Create(context.Background(), svc)
	assert.NoError(t, err)

	// Wait for the ServiceGroup to be created
	assert.Eventually(t, func() bool {
		serviceGroup := &purelb.ServiceGroup{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, serviceGroup)
		return err == nil
	}, 10*time.Second, 1*time.Second)

	// Verify the ServiceGroup spec
	var createdServiceGroup purelb.ServiceGroup
	err = k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, &createdServiceGroup)
	require.NoError(t, err)
	require.NotNil(t, createdServiceGroup.Spec.Local)
	assert.Len(t, createdServiceGroup.Spec.Local.V4Pools, 1)
	assert.Equal(t, "1.2.3.4/32", createdServiceGroup.Spec.Local.V4Pools[0].Pool)
	assert.Equal(t, "1.2.3.4/24", createdServiceGroup.Spec.Local.V4Pools[0].Subnet)
	assert.Equal(t, "default", createdServiceGroup.Spec.Local.V4Pools[0].Aggregation)

	// Delete the service
	err = k8sClient.Delete(context.Background(), svc)
	assert.NoError(t, err)

	// Wait for the ServiceGroup to be deleted
	assert.Eventually(t, func() bool {
		serviceGroup := &purelb.ServiceGroup{}
		err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "ip4-1.2.3.4", Namespace: "rancher-fip-manager"}, serviceGroup)
		return apierrors.IsNotFound(err)
	}, 10*time.Second, 1*time.Second)
}
