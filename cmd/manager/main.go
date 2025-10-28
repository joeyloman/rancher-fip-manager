package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/joeyloman/rancher-fip-manager/pkg/controller/floatingip"
	"github.com/joeyloman/rancher-fip-manager/pkg/controller/floatingippool"
	"github.com/joeyloman/rancher-fip-manager/pkg/controller/floatingipprojectquota"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	"github.com/joeyloman/rancher-fip-manager/pkg/signals"
)

var (
	kubeconfig         string
	kubeContext        string
	leaderElect        bool
	leaseLockName      string
	leaseLockNamespace string
)

var reinitChan = make(chan struct{}, 1)

func main() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&kubeContext, "context", "", "The name of the kubeconfig context to use.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election for controller.")
	flag.StringVar(&leaseLockName, "lease-lock-name", "rancher-fip-manager-lock", "The name of the leader election lock.")
	flag.StringVar(&leaseLockNamespace, "lease-lock-namespace", "rancher-fip-manager", "The namespace of the leader election lock.")
	flag.Parse()

	logrus.SetReportCaller(true)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			s := strings.Split(f.Function, ".")
			funcname := s[len(s)-1]
			_, filename := path.Split(f.File)
			packageName := path.Base(path.Dir(f.File))
			return fmt.Sprintf("(%s.%s)", packageName, funcname), fmt.Sprintf("%s:%d", filename, f.Line)
		},
	})

	// set up signals so we handle the first shutdown signal gracefully
	ctx := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		logrus.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logrus.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	fipClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		logrus.Fatalf("Error building fip clientset: %s", err.Error())
	}

	if !leaderElect {
		runLoop(ctx, kubeClient, fipClient)
		logrus.Info("Controller finished")
		return
	}

	// Leader-election logic
	id := uuid.New().String()
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseLockName,
			Namespace: leaseLockNamespace,
		},
		Client: kubeClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				runLoop(ctx, kubeClient, fipClient)
			},
			OnStoppedLeading: func() {
				logrus.Infof("leader lost: %s", id)
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					// I just became the leader
					return
				}
				logrus.Infof("new leader elected: %s", identity)
			},
		},
	})
}

func runLoop(ctx context.Context, kubeClient kubernetes.Interface, fipClient clientset.Interface) {
	for {
		runCtx, cancel := context.WithCancel(ctx)
		run(runCtx, kubeClient, fipClient, reinitChan)

		select {
		case <-reinitChan:
			logrus.Info("Re-initializing controller...")
			cancel()
		case <-ctx.Done():
			logrus.Info("Main context cancelled, stopping run loop.")
			cancel()
			return
		}
	}
}

func run(ctx context.Context, kubeClient kubernetes.Interface, fipClient clientset.Interface, reinitChan chan<- struct{}) {
	logrus.Info("Starting rancherfip controller")

	fipInformerFactory := informers.NewSharedInformerFactory(fipClient, 0)
	ipamAllocator := ipam.New()

	fipPoolInformer := fipInformerFactory.Rancher().V1beta1().FloatingIPPools()
	floatingIPProjectQuotaInformer := fipInformerFactory.Rancher().V1beta1().FloatingIPProjectQuotas()
	fipInformer := fipInformerFactory.Rancher().V1beta1().FloatingIPs()

	fipPoolController := floatingippool.New(fipClient, kubeClient, fipInformer, fipPoolInformer, floatingIPProjectQuotaInformer, ipamAllocator, reinitChan)
	floatingIPProjectQuotaController := floatingipprojectquota.New(fipClient, kubeClient, floatingIPProjectQuotaInformer, fipInformer, fipPoolInformer)
	fipController := floatingip.New(fipClient, kubeClient, fipInformer, fipPoolInformer, floatingIPProjectQuotaInformer, ipamAllocator)

	// Start informers
	go fipInformerFactory.Start(ctx.Done())

	// Start controllers
	go fipPoolController.Run(ctx, 1)
	<-fipPoolController.IsInitialSyncDone()

	go floatingIPProjectQuotaController.Run(ctx, 1)
	<-floatingIPProjectQuotaController.IsInitialSyncDone()

	go fipController.Run(ctx, 2)
	<-fipController.IsInitialSyncDone()

	<-ctx.Done()
	logrus.Info("Shutting down controller")
}
