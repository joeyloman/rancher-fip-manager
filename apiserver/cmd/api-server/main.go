package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/joeyloman/rancher-fip-api-server/internal/auth"
	"github.com/joeyloman/rancher-fip-api-server/internal/handlers"
	"github.com/joeyloman/rancher-fip-api-server/internal/kubebuilder"
	"github.com/joeyloman/rancher-fip-api-server/internal/middleware"
	"github.com/joeyloman/rancher-fip-api-server/pkg/types"
	fipv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	kubeconfig                        string
	kubecontext                       string
	jwtGenerateKey                    bool
	jwtSecretName                     string
	jwtSecretNamespace                string
	authorizedProjectsSecretName      string
	authorizedProjectsSecretNamespace string
	rateLimitPerSecond                float64
	rateLimitBurst                    int
	listenAddress                     string
	tlsEnabled                        bool
	tlsCertFile                       string
	tlsKeyFile                        string
	kubeAPIQPS                        float64
	kubeAPIBurst                      int
	maxBodySize                       int64
)

func main() {
	// Initialize logger
	log := logrus.New()
	log.SetFormatter(&logrus.JSONFormatter{})
	log.SetOutput(os.Stdout)
	log.SetLevel(logrus.InfoLevel)

	// Command-line flags
	flag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to a kubeconfig file.")
	flag.StringVar(&kubecontext, "kubecontext", os.Getenv("KUBECONTEXT"), "The name of the kubeconfig context to use.")
	// No leader election: the API server is stateless (all state lives in
	// FloatingIP CRs; reconciliation is done by the manager controller), so
	// every replica serves. Leader-only binding previously made readiness
	// probes wedge rollouts (REPORT.md 4.5).
	flag.BoolVar(&jwtGenerateKey, "jwt-generate-key", true, "Generate a new JWT private key.")
	flag.StringVar(&jwtSecretName, "jwt-secret-name", "jwt-private-key", "The name of the secret containing the JWT private key.")
	flag.StringVar(&jwtSecretNamespace, "jwt-secret-namespace", "rancher-fip-manager", "The namespace of the secret containing the JWT private key.")
	flag.Float64Var(&rateLimitPerSecond, "rate-limit-per-second", 10, "The number of requests allowed per second.")
	flag.IntVar(&rateLimitBurst, "rate-limit-burst", 20, "The burst number of requests allowed.")
	flag.StringVar(&listenAddress, "listen-address", ":8080", "The address to listen on for HTTP requests.")
	flag.BoolVar(&tlsEnabled, "tls-enabled", false, "Enable TLS for HTTPS.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "", "Path to the TLS certificate file.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "", "Path to the TLS key file.")
	flag.Float64Var(&kubeAPIQPS, "kube-api-qps", 50, "QPS for the Kubernetes client rate limiter.")
	flag.IntVar(&kubeAPIBurst, "kube-api-burst", 100, "Burst for the Kubernetes client rate limiter.")
	flag.Int64Var(&maxBodySize, "max-body-size", 1<<20, "Maximum accepted request body size in bytes.")
	flag.Parse()

	// Create root context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Info("Shutting down...")
		cancel()
	}()

	// Set up Kubernetes client
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		log.Infof("Using kubeconfig file: %s", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		log.Info("Using in-cluster config")
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Fatalf("Failed to create Kubernetes config: %v", err)
	}

	// client-go silently falls back to 5 QPS / burst 10 when QPS is unset;
	// that shared bucket is the service's real throughput ceiling under load,
	// so configure it explicitly instead.
	config.QPS = float32(kubeAPIQPS)
	config.Burst = kubeAPIBurst

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes clientset: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	run(ctx, log, config, clientset, dynamicClient)
	log.Info("Server finished")
}

func run(ctx context.Context, log *logrus.Logger, config *rest.Config, clientset *kubernetes.Clientset, dynamicClient dynamic.Interface) {
	logrus.Info("Starting rancher-fip-api-server")

	// Every replica serves unconditionally, so two pods can race to create
	// the key on first boot; EnsurePrivateKey adopts the winner's key on
	// that race — a key mismatch would silently invalidate tokens issued by
	// the other replica.
	privateKey, err := auth.EnsurePrivateKey(ctx, clientset, jwtSecretNamespace, jwtSecretName, jwtGenerateKey)
	if err != nil {
		log.Fatalf("Failed to prepare JWT private key: %v", err)
	}

	fipScheme := runtime.NewScheme()
	fipv1beta2.AddToScheme(fipScheme)

	fipRestClient, err := client.New(config, client.Options{
		Scheme: fipScheme,
	})
	if err != nil {
		log.Fatalf("Failed to create fip rest client: %v", err)
	}

	// Dedicated REST client for watching FloatingIP resources: separate from
	// the controller-runtime client so watch traffic gets its own rate-limiter
	// bucket and never queues behind handler List/Create/Update calls.
	fipWatchClient, err := kubebuilder.NewFipRestClient(config, fipScheme)
	if err != nil {
		log.Fatalf("Failed to create fip watch client: %v", err)
	}

	app := &types.App{
		FipRestClient:  fipRestClient,
		FipWatchClient: fipWatchClient,
		Clientset:      clientset,
		DynamicClient:  dynamicClient,
		Log:            log,
		PrivateKey:     privateKey,
	}

	log.Infof("Successfully connected to Kubernetes cluster: %s", clientset.Discovery().RESTClient().Get().URL())

	// Set up HTTP server
	r := mux.NewRouter()
	r.Use(middleware.RateLimitMiddleware(rateLimitPerSecond, rateLimitBurst))
	r.Use(middleware.BodyLimitMiddleware(maxBodySize))
	r.HandleFunc("/v1/auth/token", handlers.TokenHandler(app)).Methods("POST")

	// Protected routes
	s := r.PathPrefix("/v1/fip").Subrouter()
	s.Use(middleware.AuthMiddleware(app))
	s.Use(middleware.AuthorizeMiddleware(app))
	s.HandleFunc("/request", handlers.FIPRequestHandler(app)).Methods("POST")
	s.HandleFunc("/release", handlers.FIPReleaseHandler(app)).Methods("POST")
	s.HandleFunc("/delete", handlers.FIPDeleteHandler(app)).Methods("DELETE")
	s.HandleFunc("/list", handlers.FIPListHandler(app)).Methods("POST")

	server := &http.Server{
		Addr:    listenAddress,
		Handler: r,
		// Bounded server timeouts: FIPRequestHandler legitimately holds a
		// connection for up to the 30s allocation wait, so the read/write
		// budgets must exceed that, while slow clients cannot pin a
		// connection indefinitely (slowloris).
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if tlsEnabled {
			log.Infof("Starting server on %s with TLS", listenAddress)
			if err := server.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Could not listen on %s: %v\n", listenAddress, err)
			}
		} else {
			log.Infof("Starting server on %s", listenAddress)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Could not listen on %s: %v\n", listenAddress, err)
			}
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()

	// Shutdown the server gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	log.Info("Server gracefully stopped")
}
