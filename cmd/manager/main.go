package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
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
	"github.com/joeyloman/rancher-fip-manager/pkg/service"
	"github.com/joeyloman/rancher-fip-manager/pkg/signals"
)

var (
	kubeconfig             string
	kubeContext            string
	leaderElect            bool
	leaseLockName          string
	leaseLockNamespace     string
	httpServicePort        string
	healthProbeBindAddress string
)

func main() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&kubeContext, "context", "", "The name of the kubeconfig context to use.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election for controller.")
	flag.StringVar(&leaseLockName, "lease-lock-name", "rancher-fip-manager-lock", "The name of the leader election lock.")
	flag.StringVar(&leaseLockNamespace, "lease-lock-namespace", "rancher-fip-manager", "The namespace of the leader election lock.")
	flag.StringVar(&httpServicePort, "http-service-port", "8080", "The port to listen on for HTTP requests.")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "Address of the leader-election-independent /healthz probe server.")
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

	// The probe server must bind BEFORE leader election: the chart probes
	// made a candidate that only served /healthz after acquiring the lease
	// unable to go Ready while the old leader still held it - the
	// ReplicaSet never terminated the old pod and liveness killed the
	// candidate every 50s (rollout deadlock). This listener has no
	// dependencies (no listers, no API calls), so it can safely answer for
	// the standby. The /healthz on the HTTP service port stays for
	// in-cluster smoke checks.
	go serveHealthProbe(ctx, healthProbeBindAddress)

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
		run(ctx, kubeClient, fipClient)
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
				run(ctx, kubeClient, fipClient)
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

// serveHealthProbe binds the liveness/readiness endpoints independently of
// leadership, so a standby candidate passes its probes and the rollout can
// terminate the old leader (which then releases the lease). It never
// returns an error to the caller: a bind failure is logged fatally by the
// caller's supervisor via process health, matching the HTTP service.
func serveHealthProbe(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", handler)
	mux.HandleFunc("/readyz", handler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logrus.Errorf("health probe server shutdown failed: %s", err.Error())
		}
	}()
	logrus.Infof("starting health probe server on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.Errorf("health probe server failed to start on %s: %s", addr, err.Error())
	}
}

func run(ctx context.Context, kubeClient kubernetes.Interface, fipClient clientset.Interface) {
	logrus.Info("Starting rancherfip controller")

	fipInformerFactory := informers.NewSharedInformerFactory(fipClient, 0)
	ipamAllocator := ipam.New()

	fipPoolInformer := fipInformerFactory.Rancher().V1beta2().FloatingIPPools()
	floatingIPProjectQuotaInformer := fipInformerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := fipInformerFactory.Rancher().V1beta2().FloatingIPs()

	fipPoolController := floatingippool.New(fipClient, kubeClient, fipInformer, fipPoolInformer, floatingIPProjectQuotaInformer, ipamAllocator)
	floatingIPProjectQuotaController := floatingipprojectquota.New(fipClient, kubeClient, floatingIPProjectQuotaInformer, fipInformer, fipPoolInformer)
	fipController := floatingip.New(fipClient, kubeClient, fipInformer, fipPoolInformer, floatingIPProjectQuotaInformer, ipamAllocator)

	// Start the HTTP service
	httpServer := service.New(
		httpServicePort,
		ipamAllocator,
		fipPoolInformer.Lister(),
		floatingIPProjectQuotaInformer.Lister(),
		fipInformer.Lister(),
	)
	go httpServer.Start(ctx)

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
