package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	rfmv2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Handler struct {
	ctx         context.Context
	mu          sync.Mutex
	serving     bool
	httpServer  *http.Server
	clientset   kubernetes.Interface
	dynamic     dynamic.Interface
	servingCert atomic.Pointer[tls.Certificate]
}

// SetCertificate installs TLS material served by the HTTPS listener without
// touching the listener itself. net/http loads the certificate per
// handshake via GetCertificate, so a renewal is an in-memory pointer swap:
// the admission endpoint never goes down while the CSR round-trip runs
// (REPORT.md 2.4: renewal used to Stop the server, sleep 2s and restart it).
func (h *Handler) SetCertificate(pair *tls.Certificate) {
	h.servingCert.Store(pair)
}

// servingMaterialReady reports whether a TLS handshake can currently
// succeed: either renewed material was pushed in memory, or the on-disk
// pair from the last config.Run is loadable. Drives /readyz so the
// Service endpoint is only advertised while admission calls can actually be
// served, instead of failing closed against a dead handshake (5.1).
func (h *Handler) servingMaterialReady() bool {
	if h.servingCert.Load() != nil {
		return true
	}
	certPath, keyPath := certKeyPaths()
	_, err := tls.LoadX509KeyPair(certPath, keyPath)

	return err == nil
}

func certKeyPaths() (certPath string, keyPath string) {
	homedir := os.Getenv("HOME")

	return fmt.Sprintf("%s/tls.crt", homedir), fmt.Sprintf("%s/tls.key", homedir)
}

// IsServing reports whether a Run goroutine currently owns the HTTPS server.
// Run returns (serving=false) when the listener failed to bind — e.g. missing
// or unusable certificate files at startup — so the renewal scheduler knows
// to restart it once the TLS material is repaired.
func (h *Handler) IsServing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.serving
}

func (h *Handler) setServing(serving bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serving = serving
}

func Register(ctx context.Context) *Handler {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}
	return &Handler{
		ctx:       ctx,
		clientset: clientset,
		dynamic:   dynamicClient,
	}
}

func validateFloatingIP(ctx context.Context, dynamic dynamic.Interface, ar *admissionv1.AdmissionReview, fip *rfmv2.FloatingIP, oldFIP *rfmv2.FloatingIP, h *Handler) *admissionv1.AdmissionResponse {
	// Determine if this is an UPDATE operation
	isUpdate := oldFIP != nil

	// An object being deleted must always be admitted. Its release path
	// rewrites the spec (finalizer removal, bookkeeping) and must never be
	// blocked by re-validating pool range, exclusions, availability or
	// quota: an admin shrinking a pool below an assigned IP must not make
	// the FloatingIP undeletable.
	if fip.DeletionTimestamp != nil {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: true,
		}
	}

	// 1. Check if the specified FloatingIPPool exists.
	fipGVR := schema.GroupVersionResource{
		Group:    "rancher.k8s.binbash.org",
		Version:  "v1beta2",
		Resource: "floatingippools",
	}

	unstructuredFIPPool, err := dynamic.Resource(fipGVR).Get(ctx, fip.Spec.FloatingIPPool, metav1.GetOptions{})
	if err != nil {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("the specified floatingippool %s does not exist", fip.Spec.FloatingIPPool),
			},
		}
	}

	var fipPool rfmv2.FloatingIPPool
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredFIPPool.Object, &fipPool)
	if err != nil {
		log.Errorf("failed to convert unstructured FloatingIPPool to typed: %s", err)
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: "internal server error: failed to process floatingippool",
			},
		}
	}

	// 2. IP Availability
	if fip.Spec.IPAddr != nil {
		requestedIP := net.ParseIP(*fip.Spec.IPAddr)
		if requestedIP == nil {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("invalid IP address format: %s", *fip.Spec.IPAddr),
				},
			}
		}

		// Check if the IP is within the subnet
		_, subnet, err := net.ParseCIDR(fipPool.Spec.IPConfig.Subnet)
		if err != nil {
			log.Errorf("failed to parse subnet %s: %s", fipPool.Spec.IPConfig.Subnet, err)
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: "internal server error: invalid subnet configuration in floatingippool",
				},
			}
		}
		if !subnet.Contains(requestedIP) {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("requested IP %s is not in the subnet range %s", *fip.Spec.IPAddr, fipPool.Spec.IPConfig.Subnet),
				},
			}
		}

		// Check if the IP is within the fipPool.Spec.IPConfig.Pool.Start and fipPool.Spec.IPConfig.Pool.End range
		startIP := net.ParseIP(fipPool.Spec.IPConfig.Pool.Start)
		if startIP == nil {
			log.Errorf("failed to parse start IP %s from floatingippool %s", fipPool.Spec.IPConfig.Pool.Start, fip.Spec.FloatingIPPool)
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("internal server error: invalid start ip configuration in floatingippool %s", fip.Spec.FloatingIPPool),
				},
			}
		}

		endIP := net.ParseIP(fipPool.Spec.IPConfig.Pool.End)
		if endIP == nil {
			log.Errorf("failed to parse end IP %s from floatingippool %s", fipPool.Spec.IPConfig.Pool.End, fip.Spec.FloatingIPPool)
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("internal server error: invalid end ip configuration in floatingippool %s", fip.Spec.FloatingIPPool),
				},
			}
		}

		if reqIP4, startIP4, endIP4 := requestedIP.To4(), startIP.To4(), endIP.To4(); reqIP4 != nil && startIP4 != nil && endIP4 != nil {
			// All are IPv4, compare them.
			if bytes.Compare(reqIP4, startIP4) < 0 || bytes.Compare(reqIP4, endIP4) > 0 {
				return &admissionv1.AdmissionResponse{
					UID:     ar.Request.UID,
					Allowed: false,
					Result: &metav1.Status{
						Message: fmt.Sprintf("requested IP %s is not in the pool range [%s, %s]",
							*fip.Spec.IPAddr, fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End),
					},
				}
			}
		} else {
			// Compare as-is, assuming IPv6 or consistent representation from ParseIP
			if bytes.Compare(requestedIP, startIP) < 0 || bytes.Compare(requestedIP, endIP) > 0 {
				return &admissionv1.AdmissionResponse{
					UID:     ar.Request.UID,
					Allowed: false,
					Result: &metav1.Status{
						Message: fmt.Sprintf("requested IP %s is not in the pool range [%s, %s]",
							*fip.Spec.IPAddr, fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End),
					},
				}
			}
		}

		// Check if the IP is in the exclude list
		for _, excludedIP := range fipPool.Spec.IPConfig.Pool.Exclude {
			if *fip.Spec.IPAddr == excludedIP {
				return &admissionv1.AdmissionResponse{
					UID:     ar.Request.UID,
					Allowed: false,
					Result: &metav1.Status{
						Message: fmt.Sprintf("requested IP %s is in the exclude list", *fip.Spec.IPAddr),
					},
				}
			}
		}

		// Check if the IP is already allocated
		// For UPDATE operations, skip this check if the IP is the same as the old one
		allocatedIP := *fip.Spec.IPAddr
		if isUpdate && oldFIP != nil && oldFIP.Status.IPAddr == allocatedIP {
			// The IP hasn't changed, skip the allocated check
		} else if _, ok := fipPool.Status.Allocated[allocatedIP]; ok {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("requested IP %s is already allocated", *fip.Spec.IPAddr),
				},
			}
		}
	} else {
		// if no ip is requested, check if there are available ips in the pool
		if fipPool.Status.Available <= 0 {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("no available IPs in floatingippool %s", fip.Spec.FloatingIPPool),
				},
			}
		}
	}

	// Skip quota check if the IP address hasn't changed during an update
	// For auto-assignment (IPAddr is nil), we still need to check quota
	shouldCheckQuota := true
	if isUpdate && oldFIP != nil && fip.Spec.IPAddr != nil {
		allocatedIP := *fip.Spec.IPAddr
		if oldFIP.Status.IPAddr == allocatedIP {
			shouldCheckQuota = false
		}
	}

	if shouldCheckQuota {
		// 3. Project Quota Enforcement

		// This sleep prevents Quota usage race conditions when creating multiple FloatingIPs in a short period of time
		time.Sleep(2 * time.Second)

		projectID := fip.ObjectMeta.Labels["rancher.k8s.binbash.org/project-name"]

		plbcGVR := schema.GroupVersionResource{
			Group:    "rancher.k8s.binbash.org",
			Version:  "v1beta2",
			Resource: "floatingipprojectquotas",
		}

		unstructuredPLBC, err := dynamic.Resource(plbcGVR).Get(ctx, projectID, metav1.GetOptions{})
		if err != nil {
			log.Errorf("failed to get floatingipprojectquota for project %s: %s", projectID, err)
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("failed to get floatingipprojectquota for project %s", projectID),
				},
			}
		}

		var plbc rfmv2.FloatingIPProjectQuota
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredPLBC.Object, &plbc)
		if err != nil {
			log.Errorf("failed to convert unstructured FloatingIPProjectQuota to typed: %s", err)
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: "internal server error: failed to process floatingipprojectquota",
				},
			}
		}

		// Check the quota for the specified FloatingIPPool
		quota, ok := plbc.Spec.FloatingIPQuota[fip.Spec.FloatingIPPool]
		if !ok {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("no quota defined for floatingippool %s in project %s", fip.Spec.FloatingIPPool, projectID),
				},
			}
		}

		// Check the current usage for that pool
		usage := 0
		if fipInfo, ok := plbc.Status.FloatingIPs[fip.Spec.FloatingIPPool]; ok {
			usage = fipInfo.Used
		}

		if usage >= quota {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("quota exceeded for floatingippool %s in project %s. Quota: %d, Used: %d", fip.Spec.FloatingIPPool, projectID, quota, usage),
				},
			}
		}
	}

	return &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}
}

func validateFloatingIPPool(ctx context.Context, ar *admissionv1.AdmissionReview, fipPool *rfmv2.FloatingIPPool) *admissionv1.AdmissionResponse {
	// Check if the subnet is valid
	_, subnet, err := net.ParseCIDR(fipPool.Spec.IPConfig.Subnet)
	if err != nil {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("invalid subnet format: %s", fipPool.Spec.IPConfig.Subnet),
			},
		}
	}

	// Check if the start address is valid and within the subnet
	startIP := net.ParseIP(fipPool.Spec.IPConfig.Pool.Start)
	if startIP == nil {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("invalid start IP address format: %s", fipPool.Spec.IPConfig.Pool.Start),
			},
		}
	}
	if !subnet.Contains(startIP) {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("start IP address %s is not within the subnet %s", fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Subnet),
			},
		}
	}

	// Check if the end address is valid and within the subnet
	endIP := net.ParseIP(fipPool.Spec.IPConfig.Pool.End)
	if endIP == nil {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("invalid end IP address format: %s", fipPool.Spec.IPConfig.Pool.End),
			},
		}
	}
	if !subnet.Contains(endIP) {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("end IP address %s is not within the subnet %s", fipPool.Spec.IPConfig.Pool.End, fipPool.Spec.IPConfig.Subnet),
			},
		}
	}

	// Check that start <= end
	if startIP4, endIP4 := startIP.To4(), endIP.To4(); startIP4 != nil && endIP4 != nil {
		// Both are IPv4, compare them
		if bytes.Compare(startIP4, endIP4) > 0 {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("start IP address %s must be less than or equal to end IP address %s", fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End),
				},
			}
		}
	} else {
		// Compare as-is, assuming IPv6 or consistent representation from ParseIP
		if bytes.Compare(startIP, endIP) > 0 {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("start IP address %s must be less than or equal to end IP address %s", fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End),
				},
			}
		}
	}

	// Bound the pool range before it reaches the IPAM: buildSubnet
	// materializes every address in [start,end], so an oversized range must
	// be denied here with a clear message instead of leaving a pool whose
	// reconcile fails (or OOMs) forever. Runs after the ordering check, per
	// ValidateRangeSize's contract.
	if err := ipam.ValidateRangeSize(fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End); err != nil {
		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	// Check if exclude IPs are valid, within the subnet and between the start and end IP
	for _, excludedIPStr := range fipPool.Spec.IPConfig.Pool.Exclude {
		excludedIP := net.ParseIP(excludedIPStr)
		if excludedIP == nil {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("invalid excluded IP address format: %s", excludedIPStr),
				},
			}
		}
		if !subnet.Contains(excludedIP) {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result: &metav1.Status{
					Message: fmt.Sprintf("excluded IP address %s is not within the subnet %s", excludedIPStr, fipPool.Spec.IPConfig.Subnet),
				},
			}
		}
		// Check if excluded IP is outside the pool range [startIP, endIP]
		if startIP4, endIP4, excludedIP4 := startIP.To4(), endIP.To4(), excludedIP.To4(); startIP4 != nil && endIP4 != nil && excludedIP4 != nil {
			// All are IPv4, compare them
			if bytes.Compare(excludedIP4, startIP4) < 0 || bytes.Compare(excludedIP4, endIP4) > 0 {
				return &admissionv1.AdmissionResponse{
					UID:     ar.Request.UID,
					Allowed: false,
					Result: &metav1.Status{
						Message: fmt.Sprintf("excluded IP address %s is not within the pool range [%s, %s]", excludedIPStr, fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End),
					},
				}
			}
		} else {
			// Compare as-is, assuming IPv6 or consistent representation from ParseIP
			if bytes.Compare(excludedIP, startIP) < 0 || bytes.Compare(excludedIP, endIP) > 0 {
				return &admissionv1.AdmissionResponse{
					UID:     ar.Request.UID,
					Allowed: false,
					Result: &metav1.Status{
						Message: fmt.Sprintf("excluded IP address %s is not within the pool range [%s, %s]", excludedIPStr, fipPool.Spec.IPConfig.Pool.Start, fipPool.Spec.IPConfig.Pool.End),
					},
				}
			}
		}
	}

	return &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}
}

func (h *Handler) validateFloatingIPAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)
		return
	}

	fip := &rfmv2.FloatingIP{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &fip); err != nil {
		log.Errorf("cannot unmarshal json to FloatingIP: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot unmarshal json to FloatingIP: %s", err)
		return
	}

	// Handle UPDATE operations by extracting the old object
	var oldFIP *rfmv2.FloatingIP
	if ar.Request.Operation == admissionv1.Update && ar.Request.OldObject.Raw != nil {
		oldFIP = &rfmv2.FloatingIP{}
		if err := json.Unmarshal(ar.Request.OldObject.Raw, oldFIP); err != nil {
			log.Errorf("cannot unmarshal json to old FloatingIP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "cannot unmarshal json to old FloatingIP: %s", err)
			return
		}
	}

	ar.Response = validateFloatingIP(r.Context(), h.dynamic, ar, fip, oldFIP, h)
	if !ar.Response.Allowed {
		log.Warnf("(validateFloatingIPAdmission) request not allowed: %s", ar.Response.Result.Message)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

func (h *Handler) validateFloatingIPPoolAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)
		return
	}

	fipPool := &rfmv2.FloatingIPPool{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &fipPool); err != nil {
		log.Errorf("cannot unmarshal json to FloatingIPPool: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot unmarshal json to FloatingIPPool: %s", err)
		return
	}

	ar.Response = validateFloatingIPPool(r.Context(), ar, fipPool)
	if !ar.Response.Allowed {
		log.Warnf("(validateFloatingIPPoolAdmission) request not allowed: %s", ar.Response.Result.Message)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

func (h *Handler) Run() {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", h.handleReadyz)
	mux.HandleFunc("/validate-floatingip", h.validateFloatingIPAdmission)
	mux.HandleFunc("/validate-floatingippool", h.validateFloatingIPPoolAdmission)

	srv := &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Material is resolved per handshake: a certificate pushed
			// via SetCertificate takes effect on the next connection
			// without touching the listener (renewal no longer bounces
			// the admission endpoint), and before the first push the
			// files written by config.Run are loaded as before.
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				if pair := h.servingCert.Load(); pair != nil {
					return pair, nil
				}
				certPath, keyPath := certKeyPaths()
				pair, err := tls.LoadX509KeyPair(certPath, keyPath)
				if err != nil {
					return nil, err
				}

				return &pair, nil
			},
		},
	}
	h.mu.Lock()
	h.httpServer = srv
	h.mu.Unlock()

	h.setServing(true)
	defer h.setServing(false)

	if err := srv.ListenAndServeTLS("", ""); err != nil {
		if err != http.ErrServerClosed {
			log.Errorf("HTTP server error: %v", err)
		}
	}
}

// handleReadyz reports ready only while a TLS handshake can actually
// succeed. With failurePolicy Fail, a Ready webhook that cannot serve
// admissions is worse than a NotReady one: the endpoint stays advertised and
// every write is denied instead of shed.
func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.servingMaterialReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("no serving certificate"))

		return
	}
	w.Write([]byte("ok"))
}

// shutdownTimeout bounds Stop's wait for in-flight admission requests;
// overridable in tests.
var shutdownTimeout = 15 * time.Second

func (h *Handler) Stop() error {
	h.mu.Lock()
	srv := h.httpServer
	h.mu.Unlock()
	if srv == nil {
		return nil
	}

	// REPORT.md 2.5: Shutdown(h.ctx) alone ended only on SIGTERM, so a
	// wedged handler could block a renewal restart indefinitely. Deriving a
	// timeout context bounds Stop at shutdownTimeout regardless.
	ctx, cancel := context.WithTimeout(h.ctx, shutdownTimeout)
	defer cancel()

	return srv.Shutdown(ctx)
}
