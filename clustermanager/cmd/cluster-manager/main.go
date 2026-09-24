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
	kubeContext        string
	appNamespace       string
	leaderElect        bool
	leaseLockName      string
	leaseLockNamespace string
	scheme             = runtime.NewScheme()
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

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				run(ctx, cfg, appNamespace)
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
			logrus.Fatalf("Failed to get cacerts secret: %v", err)
		}
		logrus.Infof("cacerts secret not found, continuing without custom CA")
	} else {
		appConfig.CaCrt = caCerts.Data["ca.crt"]
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
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
