package main

import (
	"context"
	"crypto/tls"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
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
	healthProbeBindAddress            string
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

	// apiListenerReady gates /readyz: true only while the TLS/HTTP API
	// listener is actually accepting, so a passing readiness probe keeps
	// meaning "the service port answers" — the property the old tcpSocket
	// probe on the api port guaranteed and an unconditional /readyz would
	// have silently dropped. /healthz stays unconditional so liveness
	// works from process start.
	apiListenerReady atomic.Bool

	// shutdownGraceMargin is headroom on top of the worst-case handler
	// duration so Shutdown is not racing the last request's final write.
	shutdownGraceMargin = 5 * time.Second

	// shutdownGracePeriod is how long run() lets in-flight requests finish
	// after SIGTERM. It must cover handlers.MaxRequestDuration() — a request
	// already in flight can burn the full 30s allocation wait and then its
	// 10s cleanup — otherwise server.Shutdown hits the deadline and the
	// process exits while a handler is still writing. The
	// chart's terminationGracePeriodSeconds must exceed this, or the kubelet
	// SIGKILLs first.
	shutdownGracePeriod = handlers.MaxRequestDuration() + shutdownGraceMargin
)

// globalRateLimitFactor scales the per-client budget into the aggregate
// ceiling. 10x is far above any realistic fleet's aggregate
// load, so the ceiling only trips under churn/abuse, and it tracks the
// per-client flags instead of being a second thing to tune.
const globalRateLimitFactor = 10

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
	// probes wedge rollouts.
	flag.BoolVar(&jwtGenerateKey, "jwt-generate-key", true, "Generate a new JWT private key.")
	flag.StringVar(&jwtSecretName, "jwt-secret-name", "jwt-private-key", "The name of the secret containing the JWT private key.")
	flag.StringVar(&jwtSecretNamespace, "jwt-secret-namespace", "rancher-fip-manager", "The namespace of the secret containing the JWT private key.")
	flag.Float64Var(&rateLimitPerSecond, "rate-limit-per-second", 10, "Requests allowed per second per client (authenticated client ID on /v1/fip, source address on /v1/auth/token).")
	flag.IntVar(&rateLimitBurst, "rate-limit-burst", 20, "Burst of requests allowed per client.")
	flag.StringVar(&listenAddress, "listen-address", ":8080", "The address to listen on for HTTP requests.")
	flag.BoolVar(&tlsEnabled, "tls-enabled", false, "Enable TLS for HTTPS.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "", "Path to the TLS certificate file.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "", "Path to the TLS key file.")
	flag.Float64Var(&kubeAPIQPS, "kube-api-qps", 50, "QPS for the Kubernetes client rate limiter.")
	flag.IntVar(&kubeAPIBurst, "kube-api-burst", 100, "Burst for the Kubernetes client rate limiter.")
	flag.Int64Var(&maxBodySize, "max-body-size", 1<<20, "Maximum accepted request body size in bytes.")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "Address of the dependency-free /healthz probe server.")
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

	// The probe endpoints are served by a dependency-free listener bound
	// before the API server starts: both probes are plain HTTP on their own
	// port, so kubelet never TCP-dials the TLS listener and the log stays
	// free of the per-probe "TLS handshake error" noise.
	go serveHealthProbe(ctx, log, healthProbeBindAddress)

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

// serveHealthProbe binds the liveness/readiness endpoints independently of
// the API server: kubelet probes are plain HTTP on a dedicated port, so a
// TCP probe can never hit the TLS listener and log a handshake error per
// tick. Mirrors serveHealthProbe in cmd/manager and cmd/cluster-manager.
func serveHealthProbe(ctx context.Context, log *logrus.Logger, addr string) {
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }
	readyHandler := func(w http.ResponseWriter, _ *http.Request) {
		if !apiListenerReady.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("api listener not ready"))
			return
		}
		w.Write([]byte("ok"))
	}
	mux.HandleFunc("/healthz", handler)
	mux.HandleFunc("/readyz", readyHandler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Errorf("health probe server shutdown failed: %s", err.Error())
		}
	}()
	log.Infof("starting health probe server on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Errorf("health probe server failed to start on %s: %s", addr, err.Error())
	}
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
	// Aggregate ceiling across every key. The per-client limits below are the
	// fairness mechanism, but the client ID is caller-chosen at token issuance
	// (any clientID is minted without authentication), so a caller can mint
	// fresh IDs to get fresh per-client buckets. The ceiling caps that churn
	// while sitting far above normal aggregate traffic, so tenants do not
	// contend on it.
	r.Use(middleware.RateLimitMiddleware(
		rateLimitPerSecond*globalRateLimitFactor,
		rateLimitBurst*globalRateLimitFactor,
		middleware.GlobalRateLimitKey,
	))
	r.Use(middleware.BodyLimitMiddleware(maxBodySize))

	// Per-client limiting: one noisy tenant must not exhaust another tenant's
	// budget. The token endpoint is unauthenticated, so it is bucketed by
	// source address; the protected routes are bucketed by the client ID
	// AuthMiddleware put in the context, which is why that limiter is installed
	// after it (and before the body-reading AuthorizeMiddleware, so denied
	// requests cost nothing).
	tokenLimiter := middleware.RateLimitMiddleware(rateLimitPerSecond, rateLimitBurst, middleware.RemoteIPRateLimitKey)
	r.Handle("/v1/auth/token", tokenLimiter(http.HandlerFunc(handlers.TokenHandler(app)))).Methods("POST")

	// Protected routes
	s := r.PathPrefix("/v1/fip").Subrouter()
	s.Use(middleware.AuthMiddleware(app))
	s.Use(middleware.RateLimitMiddleware(rateLimitPerSecond, rateLimitBurst, middleware.ClientIDRateLimitKey))
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

	if tlsEnabled {
		// Serve the certificate through a GetCertificate hook instead of
		// the certFile/keyFile arguments of ServeTLS, which are read once
		// at startup: cert-manager renews the Secret ~30 days before
		// expiry, and a running pod would keep serving the old leaf until
		// it was rolled. The hook reloads when the files
		// change.
		reloader, err := newCertReloader(log, tlsCertFile, tlsKeyFile)
		if err != nil {
			log.Fatalf("Failed to load TLS certificate: %v", err)
		}
		server.TLSConfig = &tls.Config{GetCertificate: reloader.GetCertificate}
	}

	go func() {
		// Bind explicitly so apiListenerReady flips exactly when the
		// socket accepts: storing before ListenAndServe would leave a
		// window where /readyz is green but nothing is bound yet.
		ln, err := net.Listen("tcp", listenAddress)
		if err != nil {
			log.Fatalf("Could not listen on %s: %v\n", listenAddress, err)
		}
		if tlsEnabled {
			log.Infof("Starting server on %s with TLS", listenAddress)
			apiListenerReady.Store(true)
			// Empty file arguments: the key pair comes from
			// server.TLSConfig.GetCertificate (the reloading source
			// above), while ServeTLS still installs HTTP/2 and the
			// http/1.1 ALPN entry on a clone of that config.
			if err := server.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Could not serve on %s: %v\n", listenAddress, err)
			}
		} else {
			log.Infof("Starting server on %s", listenAddress)
			apiListenerReady.Store(true)
			if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Could not serve on %s: %v\n", listenAddress, err)
			}
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()

	// Shutdown the server gracefully
	// Flip readiness before draining: kubelet must stop sending traffic
	// (remove the pod from Service endpoints) at the start of the SIGTERM
	// window, not after in-flight requests have drained.
	apiListenerReady.Store(false)
	shutdownAPI(server, log, shutdownGracePeriod)

	log.Info("Server gracefully stopped")
}

// shutdownAPI drains server within grace and force-closes whatever is left
// when the deadline expires. It never terminates the process itself: an
// allocation handler already in flight may legitimately hold its connection
// for the full allocation wait plus cleanup, and exiting there resets the
// client mid-write instead of returning a clean response.
func shutdownAPI(server *http.Server, log *logrus.Logger, grace time.Duration) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), grace)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Errorf("Server shutdown did not complete within %s, force-closing remaining connections: %v", grace, err)
		if closeErr := server.Close(); closeErr != nil && closeErr != http.ErrServerClosed {
			log.Errorf("Server close failed: %v", closeErr)
		}
	}
}
