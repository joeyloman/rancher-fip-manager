/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	provisioningv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"k8s.io/client-go/rest"

	"github.com/google/uuid"
	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/config"
	rancherbinbashv2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/joeyloman/rancher-fip-cluster-manager/internal/helm"
	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/controllers"
	"github.com/joeyloman/rancher-fip-manager/pkg/signals"
)

var (
	kubeContext            string
	appNamespace           string
	leaderElect            bool
	leaseLockName          string
	leaseLockNamespace     string
	healthProbeBindAddress string
	scheme                 = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(provisioningv1.AddToScheme(scheme))
	utilruntime.Must(managementv3.AddToScheme(scheme))
	utilruntime.Must(rancherbinbashv2.AddToScheme(scheme))
}

var reinitChan = make(chan struct{}, 1)

// nolint:gocyclo
func main() {
	flag.StringVar(&kubeContext, "kubecontext", "", "The name of the kubeconfig context to use.")
	flag.StringVar(&appNamespace, "app-namespace", "rancher-fip-manager", "The namespace of the application.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election for controller.")
	flag.StringVar(&leaseLockName, "lease-lock-name", "rancher-fip-cluster-manager-lock", "The name of the leader election lock.")
	flag.StringVar(&leaseLockNamespace, "lease-lock-namespace", "rancher-fip-manager", "The namespace of the leader election lock.")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "Address the health/readiness probe server binds to.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// set up logrus logging
	level, err := logrus.ParseLevel(os.Getenv("LOGLEVEL"))
	if err != nil {
		logrus.Warnf("(main) cannot determine loglevel, leaving it on Info")
	} else {
		logrus.Infof("(main) setting loglevel to %s", level)
		logrus.SetLevel(level)
	}
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// set up signals so we handle the first shutdown signal gracefully
	ctx := signals.SetupSignalHandler()

	// Bind the probe endpoints BEFORE leader election. controller-runtime
	// only starts its probe server inside mgr.Start(), and mgr.Start is
	// gated on OnStartedLeading here - a candidate that relied on it could
	// never go Ready while the old leader held the lease, so the ReplicaSet
	// never terminated the old pod and liveness killed the candidate every
	// 50s (rollout deadlock). This dependency-free listener answers for the
	// standby; controller-runtime's own binding is disabled below.
	go serveHealthProbe(ctx, healthProbeBindAddress)

	cfg, err := ctrl.GetConfig()
	if err != nil {
		logrus.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logrus.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	if !leaderElect {
		run(ctx, cfg, appNamespace)
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

	// runStart carries the cancel func of the running controller once it is
	// up; runDone closes when run() returned (mgr.Start finished its
	// graceful shutdown). OnStoppedLeading uses them to stop the manager
	// cleanly instead of killing it mid-reconcile. client-go releases the
	// lease (ReleaseOnCancel) before OnStoppedLeading runs, so the drain
	// does not delay failover.
	runStart := make(chan context.CancelFunc, 1)
	runDone := make(chan struct{})

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// client-go cancels this context itself when the lease is
				// lost or the parent context is done (Run's deferred cancel
				// runs before OnStoppedLeading); the cancel handed over via
				// runStart is belt-and-braces so the drain works regardless
				// of client-go's internal ordering.
				runCtx, cancel := context.WithCancel(ctx)
				runStart <- cancel
				defer close(runDone)
				run(runCtx, cfg, appNamespace)
			},
			OnStoppedLeading: func() {
				logrus.Infof("leader lost: %s", id)
				drainAndExit(runStart, runDone)
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

func run(ctx context.Context, cfg *rest.Config, appNamespace string) {
	logrus.Info("Starting rancher-fip-cluster-manager")

	// Create a new clientset for the current config
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logrus.Fatalf("error creating clientset: %s", err.Error())
	}

	// Get the configmap
	configMap, err := clientset.CoreV1().ConfigMaps(appNamespace).Get(ctx, "rancher-fip-config", metav1.GetOptions{})
	if err != nil {
		// A cancelled context means shutdown raced startup (e.g. leader
		// loss before the manager came up): return instead of Fatal so the
		// process exits through the leader-loss drain with the lease
		// released, instead of os.Exit(1) while still holding the lease.
		if ctx.Err() != nil {
			logrus.Infof("(main) shutdown during startup, aborting: %v", ctx.Err())
			return
		}
		logrus.Fatalf("error getting configmap: %s", err.Error())
	}

	// Create a new config struct and populate it with the data from the configmap
	appConfig := &config.Config{
		RancherFipApiServerURL:             configMap.Data["RancherFipApiServerURL"],
		RancherFipLBControllerChartName:    configMap.Data["RancherFipLBControllerChartName"],
		RancherFipLBControllerChartRef:     configMap.Data["RancherFipLBControllerChartRef"],
		RancherFipLBControllerChartVersion: configMap.Data["RancherFipLBControllerChartVersion"],
		RancherFipLBControllerNamespace:    configMap.Data["RancherFipLBControllerNamespace"],
		RancherFipLBControllerValues:       configMap.Data["RancherFipLBControllerValues"],
		PureLBControllerChartName:          configMap.Data["PureLBControllerChartName"],
		PureLBControllerChartRef:           configMap.Data["PureLBControllerChartRef"],
		PureLBControllerChartVersion:       configMap.Data["PureLBControllerChartVersion"],
		PureLBControllerNamespace:          configMap.Data["PureLBControllerNamespace"],
		PureLBControllerValues:             configMap.Data["PureLBControllerValues"],
		MetalLBControllerChartName:         configMap.Data["MetalLBControllerChartName"],
		MetalLBControllerChartRef:          configMap.Data["MetalLBControllerChartRef"],
		MetalLBControllerChartVersion:      configMap.Data["MetalLBControllerChartVersion"],
		MetalLBControllerNamespace:         configMap.Data["MetalLBControllerNamespace"],
		MetalLBControllerValues:            configMap.Data["MetalLBControllerValues"],
	}

	// Get the cacerts secret
	caCerts, err := clientset.CoreV1().Secrets(appNamespace).Get(ctx, "cacerts", metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			if ctx.Err() != nil {
				logrus.Infof("(main) shutdown during startup, aborting: %v", ctx.Err())
				return
			}
			logrus.Fatalf("Failed to get cacerts secret: %v", err)
		}
		logrus.Infof("cacerts secret not found, continuing without custom CA")
	} else {
		appConfig.CaCrt = caCerts.Data["ca.crt"]
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Disabled: /healthz and /readyz are served by serveHealthProbe
		// (pre-election) on healthProbeBindAddress. Binding them here too
		// would collide on the same address once leadership is acquired.
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		logrus.Fatalf("unable to start manager: %s", err.Error())
	}

	if err = (&controllers.ClusterProvisioningReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		HelmClient:   helm.NewClient(),
		Config:       appConfig,
		AppNamespace: appNamespace,
		NewDownstreamClientFunc: func(config *rest.Config) (client.Client, error) {
			return client.New(config, client.Options{Scheme: mgr.GetScheme()})
		},
	}).SetupWithManager(mgr); err != nil {
		logrus.Fatalf("unable to create controller: %s", err.Error())
	}

	if err = (&controllers.FloatingIPProjectQuotaReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Config:       appConfig,
		AppNamespace: appNamespace,
	}).SetupWithManager(mgr); err != nil {
		logrus.Fatalf("unable to create controller: %s", err.Error())
	}

	logrus.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		logrus.Fatalf("problem running manager: %s", err.Error())
	}
}

// gracefulDrainTimeout bounds how long OnStoppedLeading waits for the
// controller drain. It must stay below the pod terminationGracePeriod (30s
// kubelet default), or the SIGTERM path ends in SIGKILL anyway.
const gracefulDrainTimeout = 20 * time.Second

// drainAndExit cancels the running manager's context and waits a bounded
// grace period for mgr.Start's graceful shutdown (in-flight reconciles are
// aborted via their cancelled context; the helm status plan repairs any
// leftover pending-* release records on the next leader), then exits. If
// leadership was never acquired, there is nothing to drain and it exits
// immediately.
func drainAndExit(runStart <-chan context.CancelFunc, runDone <-chan struct{}) {
	drainRun(runStart, runDone, gracefulDrainTimeout)
	os.Exit(0)
}

// drainRun cancels the manager via the cancel func received on runStart and
// waits up to grace for runDone. It reports whether the drain completed
// (also true when leadership was never acquired, i.e. nothing to drain).
func drainRun(runStart <-chan context.CancelFunc, runDone <-chan struct{}, grace time.Duration) bool {
	select {
	case cancel := <-runStart:
		cancel()
		select {
		case <-runDone:
			logrus.Info("graceful drain complete")
			return true
		case <-time.After(grace):
			logrus.Errorf("graceful drain timed out after %s, exiting anyway", grace)
			return false
		}
	default:
		// Never started leading; nothing to drain.
		return true
	}
}

// serveHealthProbe binds the liveness/readiness endpoints independently of
// leadership, so a standby candidate passes its probes and the rollout can
// terminate the old leader (which then releases the lease).
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
