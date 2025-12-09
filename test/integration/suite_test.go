package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	rancherfipv1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta1"
	fipcontroller "github.com/joeyloman/rancher-fip-manager/pkg/controller/floatingip"
	fippoolcontroller "github.com/joeyloman/rancher-fip-manager/pkg/controller/floatingippool"
	floatingipprojectquotacontroller "github.com/joeyloman/rancher-fip-manager/pkg/controller/floatingipprojectquota"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stdout), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		logf.Log.Error(err, "failed to start test environment")
		os.Exit(1)
	}

	err = rancherfipv1beta1.AddToScheme(scheme.Scheme)
	if err != nil {
		logf.Log.Error(err, "failed to add custom scheme")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		logf.Log.Error(err, "failed to create k8s client")
		os.Exit(1)
	}

	// Create resources for startup race condition test
	startupNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "startup-ns", Labels: map[string]string{"rancher.k8s.binbash.org/project-name": "startup-project"}}}
	startupProject := &rancherfipv1beta1.FloatingIPProjectQuota{ObjectMeta: metav1.ObjectMeta{Name: "startup-project"}}
	startupPool := &rancherfipv1beta1.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "startup-pool"},
		Spec: rancherfipv1beta1.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &rancherfipv1beta1.IPConfig{
				Subnet: "10.10.10.0/24",
				Pool:   rancherfipv1beta1.Pool{Start: "10.10.10.1", End: "10.10.10.10"},
			},
		},
	}
	preAllocatedIP := "10.10.10.1"
	startupFip := &rancherfipv1beta1.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-with-ip",
			Namespace: "startup-ns",
			Labels:    map[string]string{"rancher.k8s.binbash.org/project-name": "startup-project"},
		},
		Spec: rancherfipv1beta1.FloatingIPSpec{
			FloatingIPPool: "startup-pool",
			IPAddr:         &preAllocatedIP,
		},
	}

	if err := k8sClient.Create(ctx, startupNs); err != nil {
		logf.Log.Error(err, "failed to create startup test namespace")
		os.Exit(1)
	}
	if err := k8sClient.Create(ctx, startupProject); err != nil {
		logf.Log.Error(err, "failed to create startup test project")
		os.Exit(1)
	}
	if err := k8sClient.Create(ctx, startupPool); err != nil {
		logf.Log.Error(err, "failed to create startup test pool")
		os.Exit(1)
	}
	if err := k8sClient.Create(ctx, startupFip); err != nil {
		logf.Log.Error(err, "failed to create startup test fip")
		os.Exit(1)
	}

	// Pre-populate the status of the startup FIP to simulate a fully allocated IP
	// This avoids a race condition where the fip-controller tries to allocate an IP
	// that the fippool-controller has already reserved in IPAM during its startup sync.
	startupFip.Status = rancherfipv1beta1.FloatingIPStatus{
		IPAddr: preAllocatedIP,
		State:  fipStatusAllocated,
	}
	if err := k8sClient.Status().Update(ctx, startupFip); err != nil {
		logf.Log.Error(err, "failed to update startup fip status")
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logf.Log.Error(err, "failed to create kubeClient")
		os.Exit(1)
	}

	rancherfipClientSet, err := clientset.NewForConfig(cfg)
	if err != nil {
		logf.Log.Error(err, "failed to create rancherfip clientset")
		os.Exit(1)
	}

	informerFactory := informers.NewSharedInformerFactory(rancherfipClientSet, time.Second*30)
	fipInformer := informerFactory.Rancher().V1beta1().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta1().FloatingIPPools()
	floatingIPProjectQuotaInformer := informerFactory.Rancher().V1beta1().FloatingIPProjectQuotas()

	// NOTE: The constructor signatures for controllers are assumed based on the resources
	// they watch and the general controller pattern.
	ipAllocator := ipam.New()

	fipCtrl := fipcontroller.New(
		rancherfipClientSet,
		kubeClient,
		fipInformer,
		fipPoolInformer,
		floatingIPProjectQuotaInformer,
		ipAllocator,
	)
	fipPoolCtrl := fippoolcontroller.New(
		rancherfipClientSet,
		kubeClient,
		fipInformer,
		fipPoolInformer,
		floatingIPProjectQuotaInformer,
		ipAllocator,
	)
	fipProjectQuotaCtrl := floatingipprojectquotacontroller.New(
		rancherfipClientSet,
		kubeClient,
		floatingIPProjectQuotaInformer,
		fipInformer,
		fipPoolInformer,
	)

	informerFactory.Start(ctx.Done())

	logf.Log.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(ctx.Done(), fipInformer.Informer().HasSynced, fipPoolInformer.Informer().HasSynced, floatingIPProjectQuotaInformer.Informer().HasSynced); !ok {
		logf.Log.Error(nil, "failed to wait for caches to sync")
		os.Exit(1)
	}
	logf.Log.Info("Informer caches synced")

	// Mimic startup sequence from main.go
	go fipPoolCtrl.Run(ctx, 1)
	<-fipPoolCtrl.IsInitialSyncDone()

	go fipProjectQuotaCtrl.Run(ctx, 1)
	<-fipProjectQuotaCtrl.IsInitialSyncDone()

	go fipCtrl.Run(ctx, 2)
	<-fipCtrl.IsInitialSyncDone()

	code := m.Run()

	// Cleanup startup resources
	// Note: we are not checking for errors here to ensure all cleanup is attempted
	_ = k8sClient.Delete(ctx, startupFip)
	_ = k8sClient.Delete(ctx, startupPool)
	_ = k8sClient.Delete(ctx, startupProject)
	_ = k8sClient.Delete(ctx, startupNs)

	cancel()

	err = testEnv.Stop()
	if err != nil {
		logf.Log.Error(err, "failed to stop test environment")
	}

	os.Exit(code)
}
