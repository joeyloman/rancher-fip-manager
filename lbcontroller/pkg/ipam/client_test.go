package ipam

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(handler http.Handler) (*httptest.Server, *x509.CertPool) {
	ts := httptest.NewTLSServer(handler)

	certpool := x509.NewCertPool()
	certpool.AddCert(ts.Certificate())

	return ts, certpool
}

func certPEM(ts *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
}

func TestNewClient(t *testing.T) {
	ts, _ := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "{\"token\":\"test_token\"}")
	}))
	defer ts.Close()

	// Test with a valid API URL
	client, err := NewClient(ts.URL, "test_client", nil)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if client == nil {
		t.Error("expected a client, got nil")
	}

	// Test with an empty API URL
	_, err = NewClient("", "test_client", nil)
	if err == nil {
		t.Error("expected an error for empty API URL, got nil")
	}
}

// TestNewClientRejectsInvalidCA pins that an unparsable bundle is reported at
// construction time. It used to be accepted, leaving an empty root pool and a
// TLS failure only on the first request.
func TestNewClientRejectsInvalidCA(t *testing.T) {
	_, err := NewClient("https://ipam.example", "test_client", []byte("not a certificate"))
	if err == nil {
		t.Fatal("expected an error for a CA bundle without certificates, got nil")
	}
	if !strings.Contains(err.Error(), "no valid PEM certificate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCAStoreUpdateKeepsLastGoodBundleOnError(t *testing.T) {
	ts, _ := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	store, err := NewCAStore(certPEM(ts))
	if err != nil {
		t.Fatalf("failed to create CA store: %v", err)
	}
	before := store.Pool()

	if err := store.Update([]byte("garbage")); err == nil {
		t.Fatal("expected an error for an invalid CA bundle, got nil")
	}
	if store.Pool() != before {
		t.Error("an invalid update must not replace the last good bundle")
	}
}

// selfSignedPEM builds a throwaway CA certificate that no test server
// presents, to prove the client reads the rotated pool instead of a cached
// one.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ipam-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestClientTrustsRotatedCA proves that updating the store reaches an existing
// client on its next handshake, so a rotated cacerts Secret needs no restart.
func TestClientTrustsRotatedCA(t *testing.T) {
	ts, _ := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"token":"test_token"}`)
	}))
	defer ts.Close()

	serverCA := certPEM(ts)
	store, err := NewCAStore(serverCA)
	if err != nil {
		t.Fatalf("failed to create CA store: %v", err)
	}

	client, err := NewClientWithCA(ts.URL, "test_client", store)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	if _, err := client.getToken(context.Background()); err != nil {
		t.Fatalf("expected the server to be trusted, got %v", err)
	}

	// Rotate to an unrelated CA. Close the keep-alive connection so the next
	// call performs a new handshake with the new pool.
	if err := store.Update(selfSignedPEM(t)); err != nil {
		t.Fatalf("failed to rotate CA bundle: %v", err)
	}
	client.httpClient.CloseIdleConnections()
	if _, err := client.getToken(context.Background()); err == nil {
		t.Fatal("expected the server to be untrusted after rotation")
	}

	if err := store.Update(serverCA); err != nil {
		t.Fatalf("failed to restore CA bundle: %v", err)
	}
	if _, err := client.getToken(context.Background()); err != nil {
		t.Fatalf("expected the restored CA bundle to be trusted, got %v", err)
	}
}

func TestRequestFIP(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"token":"test_token"}`)
		case "/fip/request":
			w.Header().Set("Content-Type", "application/json")
			var reqBody map[string]string
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			respBody := map[string]string{
				"clientSecret":     reqBody["clientSecret"],
				"status":           "approved",
				"cluster":          reqBody["cluster"],
				"project":          reqBody["project"],
				"floatingippool":   reqBody["floatingippool"],
				"servicenamespace": reqBody["servicenamespace"],
				"servicename":      reqBody["servicename"],
				"ipaddr":           "1.2.3.4",
				"floatingipgroup":  reqBody["floatingipgroup"],
			}
			json.NewEncoder(w).Encode(respBody)
		default:
			http.NotFound(w, r)
		}
	})

	ts, certpool := newTestServer(handler)
	defer ts.Close()

	caCertPEM := certPEM(ts)

	client, err := NewClient(ts.URL, "test_client", caCertPEM)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: certpool,
		},
	}

	ip, _, _, err := client.RequestFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "", "default")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if ip != "1.2.3.4" {
		t.Errorf("expected ip 1.2.3.4, got %s", ip)
	}
}

// TestRequestFIPGroupValidation pins the group check in RequestFIP. A Service
// without a FloatingIPGroup sends an empty group and the manager omits the
// field from its response (omitempty); that must be accepted. A response
// carrying a different group than requested must be rejected, otherwise the
// controller would record an address belonging to another Service's group.
func TestRequestFIPGroupValidation(t *testing.T) {
	tests := []struct {
		name              string
		requestGroup      string
		responseGroup     string
		omitResponseGroup bool
		wantErr           string
	}{
		{
			name:              "empty request group accepts omitted response group",
			requestGroup:      "",
			omitResponseGroup: true,
		},
		{
			name:          "mismatched response group is rejected",
			requestGroup:  "group-a",
			responseGroup: "group-b",
			wantErr:       "floatingip-group name mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/token" {
					fmt.Fprintln(w, `{"token":"test_token"}`)
					return
				}

				var reqBody map[string]string
				if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				respBody := map[string]string{
					"clientSecret":     reqBody["clientSecret"],
					"status":           "approved",
					"cluster":          reqBody["cluster"],
					"project":          reqBody["project"],
					"floatingippool":   reqBody["floatingippool"],
					"servicenamespace": reqBody["servicenamespace"],
					"servicename":      reqBody["servicename"],
					"ipaddr":           "1.2.3.4",
				}
				if !tc.omitResponseGroup {
					respBody["floatingipgroup"] = tc.responseGroup
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(respBody)
			})

			ts, _ := newTestServer(handler)
			t.Cleanup(ts.Close)

			client, err := NewClient(ts.URL, "test_client", nil)
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			client.httpClient.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}

			_, _, _, err = client.RequestFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "", tc.requestGroup)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRequestFIPFailures(t *testing.T) {
	// Test case 1: Token endpoint fails
	handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			http.Error(w, "token error", http.StatusInternalServerError)
		}
	})
	ts1, _ := newTestServer(handler1)
	client1, _ := NewClient(ts1.URL, "test_client", nil)
	client1.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, _, _, err := client1.RequestFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "", "default")
	if err == nil {
		t.Error("expected an error for token failure, got nil")
	}
	ts1.Close()

	// Test case 2: Request FIP endpoint returns non-200
	handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/request" {
			http.Error(w, "quota exceeded", http.StatusForbidden)
		}
	})
	ts2, _ := newTestServer(handler2)
	client2, _ := NewClient(ts2.URL, "test_client", nil)
	client2.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, _, _, err = client2.RequestFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "", "default")
	if err == nil {
		t.Error("expected an error for non-200 response, got nil")
	}
	ts2.Close()

	// Test case 3: Malformed JSON response
	handler3 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/request" {
			fmt.Fprintln(w, `{"ipaddr":malformed}`)
		}
	})
	ts3, _ := newTestServer(handler3)
	client3, _ := NewClient(ts3.URL, "test_client", nil)
	client3.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, _, _, err = client3.RequestFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "", "default")
	if err == nil {
		t.Error("expected an error for malformed JSON, got nil")
	}
	ts3.Close()

	// Test case 4: Field mismatch
	handler4 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/request" {
			w.Header().Set("Content-Type", "application/json")
			var reqBody map[string]string
			json.NewDecoder(r.Body).Decode(&reqBody)
			respBody := map[string]string{
				"clientSecret":     "wrong-secret",
				"status":           "approved",
				"cluster":          reqBody["cluster"],
				"project":          reqBody["project"],
				"floatingippool":   reqBody["floatingippool"],
				"servicenamespace": reqBody["servicenamespace"],
				"servicename":      reqBody["servicename"],
				"ipaddr":           "1.2.3.4",
				"floatingipgroup":  reqBody["floatingipgroup"],
			}
			json.NewEncoder(w).Encode(respBody)
		}
	})
	ts4, _ := newTestServer(handler4)
	client4, _ := NewClient(ts4.URL, "test_client", nil)
	client4.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, _, _, err = client4.RequestFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "", "default")
	if err == nil {
		t.Error("expected an error for field mismatch, got nil")
	}
	ts4.Close()
}

func TestReleaseFIP(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"token":"test_token"}`)
		case "/fip/release":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"status":"released"}`)
		default:
			http.NotFound(w, r)
		}
	})

	ts, _ := newTestServer(handler)
	defer ts.Close()

	client, _ := NewClient(ts.URL, "test_client", nil)
	client.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	err := client.ReleaseFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "1.2.3.4", "default")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestReleaseFIPFailures(t *testing.T) {
	// Test case 1: Token endpoint fails
	handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			http.Error(w, "token error", http.StatusInternalServerError)
		}
	})
	ts1, _ := newTestServer(handler1)
	client1, _ := NewClient(ts1.URL, "test_client", nil)
	client1.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	err := client1.ReleaseFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "1.2.3.4", "default")
	if err == nil {
		t.Error("expected an error for token failure, got nil")
	}
	ts1.Close()

	// Test case 2: Release FIP endpoint returns non-200
	handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/release" {
			http.Error(w, "error", http.StatusInternalServerError)
		}
	})
	ts2, _ := newTestServer(handler2)
	client2, _ := NewClient(ts2.URL, "test_client", nil)
	client2.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	err = client2.ReleaseFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "1.2.3.4", "default")
	if err == nil {
		t.Error("expected an error for non-200 response, got nil")
	}
	ts2.Close()

	// Test case 3: Status not "released"
	handler3 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/release" {
			fmt.Fprintln(w, `{"status":"failed"}`)
		}
	})
	ts3, _ := newTestServer(handler3)
	client3, _ := NewClient(ts3.URL, "test_client", nil)
	client3.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	err = client3.ReleaseFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "1.2.3.4", "default")
	if err == nil {
		t.Error("expected an error for status not released, got nil")
	}
	ts3.Close()
}

// TestReleaseFIPAlreadyReleased verifies that release is idempotent. Once the
// manager has released an address it drops the service labels, so a retry
// (crash, failed annotation/finalizer update) gets 404 instead of 200. That
// must be success, otherwise the Service is stuck in Terminating forever.
func TestReleaseFIPAlreadyReleased(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/token" {
					fmt.Fprintln(w, `{"token":"test_token"}`)
					return
				}
				http.Error(w, "no floatingip found to release", status)
			})
			ts, _ := newTestServer(handler)
			defer ts.Close()

			client, _ := NewClient(ts.URL, "test_client", nil)
			client.httpClient.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}

			if err := client.ReleaseFIP(context.Background(), "secret", "cluster", "project", "pool", "ns", "service", "1.2.3.4", "default"); err != nil {
				t.Errorf("expected no error for status %d, got %v", status, err)
			}
		})
	}
}

func TestListFIPs(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"token":"test_token"}`)
		case "/fip/list":
			w.Header().Set("Content-Type", "application/json")
			resp := FIPListResponse{
				ClientSecret: "secret",
				Cluster:      "cluster",
				Project:      "project",
				FloatingIPs: []FloatingIP{
					{
						Project:          "project",
						Cluster:          "cluster",
						FloatingIPPool:   "pool",
						ServiceNamespace: "ns",
						ServiceName:      "service",
						IPAddress:        "1.2.3.4",
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	})

	ts, _ := newTestServer(handler)
	defer ts.Close()

	client, _ := NewClient(ts.URL, "test_client", nil)
	client.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	fipList, err := client.ListFIPs(context.Background(), "secret", "cluster", "project", "pool")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(fipList.FloatingIPs) != 1 {
		t.Errorf("expected 1 floating IP, got %d", len(fipList.FloatingIPs))
	}
	if fipList.FloatingIPs[0].IPAddress != "1.2.3.4" {
		t.Errorf("expected IP 1.2.3.4, got %s", fipList.FloatingIPs[0].IPAddress)
	}
}

func TestListFIPsFailures(t *testing.T) {
	// Test case 1: Token endpoint fails
	handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			http.Error(w, "token error", http.StatusInternalServerError)
		}
	})
	ts1, _ := newTestServer(handler1)
	client1, _ := NewClient(ts1.URL, "test_client", nil)
	client1.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, err := client1.ListFIPs(context.Background(), "secret", "cluster", "project", "pool")
	if err == nil {
		t.Error("expected an error for token failure, got nil")
	}
	ts1.Close()

	// Test case 2: List FIPs endpoint returns non-200
	handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/list" {
			http.Error(w, "error", http.StatusInternalServerError)
		}
	})
	ts2, _ := newTestServer(handler2)
	client2, _ := NewClient(ts2.URL, "test_client", nil)
	client2.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, err = client2.ListFIPs(context.Background(), "secret", "cluster", "project", "pool")
	if err == nil {
		t.Error("expected an error for non-200 response, got nil")
	}
	ts2.Close()

	// Test case 3: Malformed JSON response
	handler3 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/list" {
			fmt.Fprintln(w, `{"floatingips":malformed}`)
		}
	})
	ts3, _ := newTestServer(handler3)
	client3, _ := NewClient(ts3.URL, "test_client", nil)
	client3.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	_, err = client3.ListFIPs(context.Background(), "secret", "cluster", "project", "pool")
	if err == nil {
		t.Error("expected an error for malformed JSON, got nil")
	}
	ts3.Close()
}

func TestDeleteFIP(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"token":"test_token"}`)
		case "/fip/delete":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})

	ts, _ := newTestServer(handler)
	defer ts.Close()

	client, _ := NewClient(ts.URL, "test_client", nil)
	client.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	err := client.DeleteFIP(context.Background(), "secret", "project", "1.2.3.4")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestDeleteFIPFailures(t *testing.T) {
	// Test case 1: Token endpoint fails
	handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			http.Error(w, "token error", http.StatusInternalServerError)
		}
	})
	ts1, _ := newTestServer(handler1)
	client1, _ := NewClient(ts1.URL, "test_client", nil)
	client1.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	err := client1.DeleteFIP(context.Background(), "secret", "project", "1.2.3.4")
	if err == nil {
		t.Error("expected an error for token failure, got nil")
	}
	ts1.Close()

	// Test case 2: Delete FIP endpoint returns non-200
	handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			fmt.Fprintln(w, `{"token":"test_token"}`)
		} else if r.URL.Path == "/fip/delete" {
			http.Error(w, "error", http.StatusInternalServerError)
		}
	})
	ts2, _ := newTestServer(handler2)
	client2, _ := NewClient(ts2.URL, "test_client", nil)
	client2.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	err = client2.DeleteFIP(context.Background(), "secret", "project", "1.2.3.4")
	if err == nil {
		t.Error("expected an error for non-200 response, got nil")
	}
	ts2.Close()
}
