package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joeyloman/rancher-fip-lb-controller/internal/controller"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/http"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/ipam"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/metallb"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/purelb"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/util"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	// leaderLabelKey marks the pod that serves the HTTP management UI. The
	// chart's Service selects on it so UI traffic only reaches the replica
	// that can act on the cluster.
	leaderLabelKey = "rancher-fip-lb-controller/leader"

	// uiStartupTimeout bounds how long a new leader waits for the management
	// UI listener before it stops advertising itself.
	uiStartupTimeout = 30 * time.Second

	// leaderDrainTimeout bounds how long OnStoppedLeading waits for the
	// controller's own drain (20s grace plus 5s abort). Together with the 5s
	// label removal it stays within the pod's 30s termination grace period.
	leaderDrainTimeout = 25 * time.Second

	// caSecretName is the Secret in the app namespace holding the CA bundle
	// used to verify the IPAM manager.
	caSecretName = "cacerts"

	// caReloadInterval bounds how long a rotated CA bundle takes to reach the
	// running clients. Rotation must not require a restart.
	caReloadInterval = time.Minute
)

var (
	kubeConfig         string
	kubeContext        string
	appNamespace       string
	leaderElect        bool
	leaseLockName      string
	leaseLockNamespace string
	logLevel           string
	httpServerEnabled  bool
	httpServerPort     int
	healthPort         int
	workers            int
)

func main() {
	// Initialize logger
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	flag.StringVar(&kubeConfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to a kubeconfig file.")
	flag.StringVar(&kubeContext, "kubecontext", os.Getenv("KUBECONTEXT"), "The name of the kubeconfig context to use.")
	flag.StringVar(&appNamespace, "app-namespace", "rancher-fip-manager", "The namespace of the application.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election for controller.")
	flag.StringVar(&leaseLockName, "lease-lock-name", "rancher-fip-lb-controller-lock", "The name of the leader election lock.")
	flag.StringVar(&leaseLockNamespace, "lease-lock-namespace", "rancher-fip-manager", "The namespace of the leader election lock.")
	flag.StringVar(&logLevel, "log-level", "info", "The level of logging (e.g. debug, info, warn, error).")
	flag.BoolVar(&httpServerEnabled, "enable-http-server", true, "Enable the HTTP server for FIP management.")
	flag.IntVar(&httpServerPort, "http-server-port", 8080, "The port of the HTTP server listening for FIP management requests.")
	flag.IntVar(&healthPort, "health-port", 8081, "The port of the health and readiness probe server.")
	flag.IntVar(&workers, "workers", 4, "Number of concurrent Service reconcile workers. One worker serializes every allocation behind a slow reconcile.")
	flag.Parse()

	if workers < 1 {
		logrus.Fatalf("--workers must be at least 1, got %d", workers)
	}

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Fatalf("Failed to parse log level: %v", err)
	}
	logrus.SetLevel(level)

	// Create root context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logrus.Info("Shutting down...")
		cancel()
	}()

	// Set up Kubernetes client
	var config *rest.Config
	var configErr error
	if kubeConfig != "" {
		logrus.Infof("Using kubeconfig file: %s", kubeConfig)
		config, configErr = clientcmd.BuildConfigFromFlags("", kubeConfig)
	} else {
		logrus.Info("Using in-cluster config")
		config, configErr = rest.InClusterConfig()
	}
	if configErr != nil {
		logrus.Fatalf("Failed to create Kubernetes config: %v", configErr)
	}

	clientset, clientsetErr := kubernetes.NewForConfig(config)
	if clientsetErr != nil {
		logrus.Fatalf("Failed to create Kubernetes clientset: %v", clientsetErr)
	}

	// Health probes must be available on every replica, independently of
	// leader election and of whether the management UI is configured.
	go func() {
		if err := http.ServeHealth(ctx, healthPort); err != nil {
			logrus.Errorf("health server stopped: %s", err)
		}
	}()

	// Read the CA certificate from the secret. The bundle is reloaded in the
	// background, so a transient API error here must not take the process
	// down: TLS calls fail until the Secret becomes readable.
	var caCertData []byte
	caCert, err := clientset.CoreV1().Secrets(appNamespace).Get(ctx, caSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logrus.Infof("cacerts secret not found, continuing without custom CA")
		} else {
			logrus.Warnf("failed to get cacerts secret, retrying in the background: %v", err)
		}
	} else {
		caCertData = caCert.Data["ca.crt"]
	}

	// An unparsable bundle is a configuration error, not a transient one:
	// fail fast with a precise message instead of failing every handshake.
	caStore, err := ipam.NewCAStore(caCertData)
	if err != nil {
		logrus.Fatalf("Invalid CA bundle in cacerts secret: %v", err)
	}
	go watchCACert(ctx, clientset, appNamespace, caStore)

	metallbClient, metallbErr := metallb.NewClient(config)
	if metallbErr != nil {
		logrus.Fatalf("Failed to create MetalLB client: %v", metallbErr)
	}

	purelbClient, purelbErr := purelb.NewClient(config)
	if purelbErr != nil {
		logrus.Fatalf("Failed to create PureLB client: %v", purelbErr)
	}

	// The HTTP UI runs on every replica. Mutating requests are gated on
	// leadership and the Service selects the leader pod by label, so the UI
	// stays reachable across leader changes instead of being bound to the
	// pod that happened to win the first election.
	var leaderReady atomic.Bool
	isLeader := func() bool { return leaderReady.Load() }

	var uiReady <-chan struct{}
	if httpServerEnabled {
		ready, err := startHttpServer(ctx, clientset, appNamespace, caStore, httpServerPort, isLeader)
		if err != nil {
			logrus.Errorf("failed to start HTTP server: %s", err)
		}
		uiReady = ready
	}

	podName := os.Getenv("POD_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		podNamespace = appNamespace
	}

	// A previous container in this pod may have left the leader label behind;
	// do not advertise a UI that is not serving yet.
	removeLeaderLabel(ctx, clientset, podName, podNamespace)

	publishLeader := func(ctx context.Context) {
		go markLeaderPod(ctx, clientset, podName, podNamespace, uiReady)
	}

	if !leaderElect {
		leaderReady.Store(true)
		publishLeader(ctx)
		run(ctx, clientset, metallbClient, purelbClient, appNamespace, caStore)
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
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// controllerStarted and controllerDone let OnStoppedLeading wait for the
	// leader's controller to drain instead of exiting mid-reconcile.
	controllerStarted := make(chan struct{})
	controllerDone := make(chan struct{})

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				close(controllerStarted)
				leaderReady.Store(true)
				publishLeader(ctx)
				run(ctx, clientset, metallbClient, purelbClient, appNamespace, caStore)
				close(controllerDone)
			},
			OnStoppedLeading: func() {
				leaderReady.Store(false)
				// Best-effort: stop routing UI traffic to this replica. Run it
				// concurrently with the drain so the 5s bound does not add to
				// the drain wait and push the process past the pod's 30s
				// termination grace period.
				unlabelDone := make(chan struct{})
				go func() {
					defer close(unlabelDone)
					unlabelCtx, unlabelCancel := context.WithTimeout(context.Background(), 5*time.Second)
					removeLeaderLabel(unlabelCtx, clientset, podName, podNamespace)
					unlabelCancel()
				}()

				// Run drains in-flight reconciles after its context is
				// canceled; exiting here without waiting would abandon a
				// reconcile between RequestFIP and the annotation write,
				// leaking the address on the next start.
				select {
				case <-controllerStarted:
					select {
					case <-controllerDone:
					case <-time.After(leaderDrainTimeout):
						logrus.Warnf("controller did not drain within %s, exiting", leaderDrainTimeout)
					}
				default:
					// The controller never started; there is nothing to drain.
				}
				<-unlabelDone

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

func run(ctx context.Context, clientset *kubernetes.Clientset, metallbClient *metallb.Client, purelbClient *purelb.Client, appNamespace string, caStore *ipam.CAStore) {
	logrus.Info("Starting rancher-fip-lb-controller")

	c := controller.New(clientset, metallbClient, purelbClient, appNamespace, caStore)
	c.Run(ctx, workers)
}

// watchCACert keeps the in-memory CA bundle in sync with the cacerts Secret.
// Every IPAM client reads the bundle from this store on each TLS handshake, so
// a rotated certificate is trusted within caReloadInterval and a Secret that
// was unreadable at startup is picked up as soon as the API server recovers.
// An invalid bundle is logged and ignored so the last known
// good bundle stays in effect.
func watchCACert(ctx context.Context, clientset kubernetes.Interface, appNamespace string, store *ipam.CAStore) {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		caCert, err := clientset.CoreV1().Secrets(appNamespace).Get(ctx, caSecretName, metav1.GetOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				logrus.Warnf("failed to reload cacerts secret: %v", err)
			}
			return
		}
		if err := store.Update(caCert.Data["ca.crt"]); err != nil {
			logrus.Errorf("ignoring invalid CA bundle in cacerts secret: %v", err)
		}
	}, caReloadInterval)
}

func startHttpServer(ctx context.Context, clientset *kubernetes.Clientset, appNamespace string, caStore *ipam.CAStore, httpServerPort int, isLeader func() bool) (<-chan struct{}, error) {
	// Only start the HTTP server if the app namespace has a project ID label
	appNs, err := clientset.CoreV1().Namespaces().Get(ctx, appNamespace, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get app namespace %s: %v", appNamespace, err)
	}

	projectId, ok := appNs.Labels["rancher.k8s.binbash.org/project-name"]
	if !ok {
		// This is not an error, it just means that the HTTP server will not be started because it's not part of a project (bare-metal cluster)
		logrus.Warnf("namespace %s does not have a project ID label 'rancher.k8s.binbash.org/project-name', aborting HTTP server", appNamespace)
		return nil, nil
	}

	// generate username and password for http basic auth
	httpAuthSecretName := "rancher-fip-http-auth"
	httpAuthSecret, err := clientset.CoreV1().Secrets(appNamespace).Get(ctx, httpAuthSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logrus.Infof("HTTP auth secret %s not found, creating it", httpAuthSecretName)

			username, err := util.GenerateRandomString(10)
			if err != nil {
				return nil, fmt.Errorf("failed to generate random string for username: %s", err)
			}
			password, err := util.GenerateRandomString(32)
			if err != nil {
				return nil, fmt.Errorf("failed to generate random string for password: %s", err)
			}

			httpAuthSecret, err = clientset.CoreV1().Secrets(appNamespace).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      httpAuthSecretName,
					Namespace: appNamespace,
				},
				StringData: map[string]string{
					"username": username,
					"password": password,
				},
			}, metav1.CreateOptions{})
			if errors.IsAlreadyExists(err) {
				// Another replica created the shared credentials first.
				httpAuthSecret, err = clientset.CoreV1().Secrets(appNamespace).Get(ctx, httpAuthSecretName, metav1.GetOptions{})
			}
			if err != nil {
				return nil, fmt.Errorf("failed to create http auth secret %s: %s", httpAuthSecretName, err)
			}
		} else {
			return nil, fmt.Errorf("failed to get http auth secret %s: %s", httpAuthSecretName, err)
		}
	}

	username := string(httpAuthSecret.Data["username"])
	password := string(httpAuthSecret.Data["password"])

	secretName := fmt.Sprintf("rancher-fip-config-%s", projectId)
	secret, err := clientset.CoreV1().Secrets(appNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s in namespace %s: %v", secretName, appNamespace, err)
	}

	ipamAPIURL := string(secret.Data["apiUrl"])
	clientSecret := string(secret.Data["clientSecret"])
	cluster := string(secret.Data["cluster"])
	project := string(secret.Data["project"])
	ipamClientID := fmt.Sprintf("client-%s", cluster)

	configMap, err := clientset.CoreV1().ConfigMaps(appNamespace).Get(ctx, "network-interface-mappings", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap network-interface-mappings: %w", err)
	}

	var floatingIPPools []string
	for pool := range configMap.Data {
		floatingIPPools = append(floatingIPPools, pool)
	}

	ipamClient, err := ipam.NewClientWithCA(ipamAPIURL, ipamClientID, caStore)
	if err != nil {
		return nil, fmt.Errorf("failed to create ipam client: %w", err)
	}

	httpServer, err := http.NewServer(ipamClient, clientset, clientSecret, cluster, project, floatingIPPools, username, password, isLeader)
	if err != nil {
		return nil, fmt.Errorf("failed to create http server: %w", err)
	}

	go func() {
		if err := httpServer.Start(ctx, httpServerPort); err != nil {
			logrus.Errorf("HTTP server stopped: %s", err)
		}
	}()

	return httpServer.Ready(), nil
}

// markLeaderPod labels this pod as the HTTP UI leader once the UI listener is
// accepting connections. The chart's Service selects on that label, so it must
// not be set before the port is open.
func markLeaderPod(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace string, uiReady <-chan struct{}) {
	if podName == "" {
		logrus.Warn("POD_NAME is not set; the HTTP UI Service will not select this pod")
		return
	}
	if uiReady == nil {
		logrus.Warn("HTTP server is not running; not advertising this pod as the UI leader")
		return
	}

	select {
	case <-uiReady:
	case <-time.After(uiStartupTimeout):
		logrus.Warnf("HTTP server did not start within %s; not advertising pod %s/%s as the UI leader", uiStartupTimeout, podNamespace, podName)
		return
	case <-ctx.Done():
		return
	}

	if err := patchLeaderLabel(ctx, clientset, podName, podNamespace, true); err != nil {
		logrus.Errorf("failed to mark pod %s/%s as UI leader: %s", podNamespace, podName, err)
		return
	}
	logrus.Infof("pod %s/%s marked as UI leader", podNamespace, podName)
}

// removeLeaderLabel clears the UI leader label, best effort.
func removeLeaderLabel(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace string) {
	if podName == "" {
		return
	}
	if err := patchLeaderLabel(ctx, clientset, podName, podNamespace, false); err != nil {
		logrus.Debugf("could not remove UI leader label from pod %s/%s: %s", podNamespace, podName, err)
	}
}

func patchLeaderLabel(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace string, leader bool) error {
	// A nil value removes the label; a string is required for a label value.
	var value any
	if leader {
		value = "true"
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{leaderLabelKey: value},
		},
	})
	if err != nil {
		return err
	}
	_, err = clientset.CoreV1().Pods(podNamespace).Patch(ctx, podName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
