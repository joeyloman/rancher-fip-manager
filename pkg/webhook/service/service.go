package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	rfmv2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
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
	ctx        context.Context
	httpServer *http.Server
	clientset  kubernetes.Interface
	dynamic    dynamic.Interface
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
	homedir := os.Getenv("HOME")
	keyPath := fmt.Sprintf("%s/tls.key", homedir)
	certPath := fmt.Sprintf("%s/tls.crt", homedir)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/validate-floatingip", h.validateFloatingIPAdmission)
	mux.HandleFunc("/validate-floatingippool", h.validateFloatingIPPoolAdmission)

	h.httpServer = &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	if err := h.httpServer.ListenAndServeTLS(certPath, keyPath); err != nil {
		if err != http.ErrServerClosed {
			log.Errorf("HTTP server error: %v", err)
		}
	}
}

func (h *Handler) Stop() error {
	return h.httpServer.Shutdown(h.ctx)
}
