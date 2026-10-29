package http

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joeyloman/rancher-fip-lb-controller/pkg/ipam"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// newAuthenticatedServer returns a server plus the session cookie and CSRF
// token a browser would present after logging in.
func newAuthenticatedServer(t *testing.T, ipamClient *ipam.Client, clientset kubernetes.Interface) (*Server, *http.Cookie, string) {
	t.Helper()

	server, err := NewServer(ipamClient, clientset, "secret", "cluster", "project", []string{"pool1"}, "testuser", "testpassword", func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	const (
		sessionToken = "test-session-token"
		csrfToken    = "test-csrf-token"
	)
	server.sessions[sessionToken] = session{expiresAt: time.Now().Add(time.Hour), csrfToken: csrfToken}

	return server, &http.Cookie{Name: sessionCookieName, Value: sessionToken}, csrfToken
}

func TestHandleFIPList(t *testing.T) {
	mockIpamAPI := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Write([]byte(`{"token":"test-token"}`))
		case "/fip/list":
			fipList := ipam.FIPListResponse{
				FloatingIPs: []ipam.FloatingIP{
					{Project: "test-project", Cluster: "test-cluster", IPAddress: "1.2.3.4"},
				},
			}
			json.NewEncoder(w).Encode(fipList)
		default:
			http.NotFound(w, r)
		}
	})
	defer mockIpamAPI.Close()

	ipamClient, err := ipam.NewClient(mockIpamAPI.URL, "test-client", nil)
	if err != nil {
		t.Fatal(err)
	}

	server, cookie, csrfToken := newAuthenticatedServer(t, ipamClient, fake.NewSimpleClientset())

	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleFIPList)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	expected := "<td>test-project</td>"
	if !strings.Contains(rr.Body.String(), expected) {
		t.Errorf("handler returned unexpected body: got %v want body to contain %v",
			rr.Body.String(), expected)
	}

	if !strings.Contains(rr.Body.String(), `name="csrf_token" value="`+csrfToken+`"`) {
		t.Errorf("handler did not render the session CSRF token")
	}
}

func TestHandleRelease(t *testing.T) {
	var releasedProject, releasedCluster string
	mockIpamAPI := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Write([]byte(`{"token":"test-token"}`))
		case "/fip/release":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("failed to decode release request: %v", err)
			}
			releasedProject = body["project"]
			releasedCluster = body["cluster"]
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"released"}`))
		default:
			http.NotFound(w, r)
		}
	})
	defer mockIpamAPI.Close()

	ipamClient, err := ipam.NewClient(mockIpamAPI.URL, "test-client", nil)
	if err != nil {
		t.Fatal(err)
	}

	server, cookie, csrfToken := newAuthenticatedServer(t, ipamClient, fake.NewSimpleClientset())

	// Project and cluster are attacker-controlled form values here: the
	// handler must ignore them and use the configured identity.
	formValues := "csrf_token=" + csrfToken + "&project=other&cluster=other&floatingippool=f&servicenamespace=s&servicename=s&ipaddr=1.2.3.4&floatingipgroup=default"
	req, err := http.NewRequest("POST", "/release", strings.NewReader(formValues))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRelease)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusSeeOther {
		t.Errorf("handler returned wrong status code: got %v want %v, body: %s",
			status, http.StatusSeeOther, rr.Body.String())
	}

	if releasedProject != "project" || releasedCluster != "cluster" {
		t.Errorf("release used form identity: project=%q cluster=%q, want configured project/cluster", releasedProject, releasedCluster)
	}
}

func TestHandleRelease_ManagedServiceDeleted(t *testing.T) {
	mockIpamAPI := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Write([]byte(`{"token":"test-token"}`))
		case "/fip/release":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"released"}`))
		default:
			http.NotFound(w, r)
		}
	})
	defer mockIpamAPI.Close()

	ipamClient, err := ipam.NewClient(mockIpamAPI.URL, "test-client", nil)
	if err != nil {
		t.Fatal(err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "s",
			Name:      "s",
			Annotations: map[string]string{
				floatingIPAnnotation: "1.2.3.4",
				projectAnnotation:    "project",
			},
		},
	}
	clientset := fake.NewSimpleClientset(svc)
	server, cookie, csrfToken := newAuthenticatedServer(t, ipamClient, clientset)

	formValues := "csrf_token=" + csrfToken + "&floatingippool=f&servicenamespace=s&servicename=s&ipaddr=1.2.3.4&floatingipgroup=default"
	req, err := http.NewRequest("POST", "/release", strings.NewReader(formValues))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRelease)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusSeeOther {
		t.Fatalf("handler returned wrong status code: got %v want %v, body: %s",
			status, http.StatusSeeOther, rr.Body.String())
	}

	if _, err := clientset.CoreV1().Services("s").Get(context.Background(), "s", metav1.GetOptions{}); err == nil {
		t.Errorf("managed service was not deleted")
	}
}

func TestHandleRelease_UnmanagedServiceRejected(t *testing.T) {
	ipamClient, err := ipam.NewClient("http://127.0.0.1:1", "test-client", nil)
	if err != nil {
		t.Fatal(err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "s", Name: "s"},
	}
	clientset := fake.NewSimpleClientset(svc)
	server, cookie, csrfToken := newAuthenticatedServer(t, ipamClient, clientset)

	formValues := "csrf_token=" + csrfToken + "&floatingippool=f&servicenamespace=s&servicename=s&ipaddr=1.2.3.4&floatingipgroup=default"
	req, err := http.NewRequest("POST", "/release", strings.NewReader(formValues))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRelease)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusConflict {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusConflict)
	}

	if _, err := clientset.CoreV1().Services("s").Get(context.Background(), "s", metav1.GetOptions{}); err != nil {
		t.Errorf("unmanaged service was deleted: %v", err)
	}
}

func TestHandleRelease_CSRFRejected(t *testing.T) {
	server, cookie, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	formValues := "csrf_token=wrong&floatingippool=f&servicenamespace=s&servicename=s&ipaddr=1.2.3.4&floatingipgroup=default"
	req, err := http.NewRequest("POST", "/release", strings.NewReader(formValues))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRelease)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusForbidden {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusForbidden)
	}
}

func TestHandleRemove(t *testing.T) {
	mockIpamAPI := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			w.Write([]byte(`{"token":"test-token"}`))
		case "/fip/delete":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	defer mockIpamAPI.Close()

	ipamClient, err := ipam.NewClient(mockIpamAPI.URL, "test-client", nil)
	if err != nil {
		t.Fatal(err)
	}

	server, cookie, csrfToken := newAuthenticatedServer(t, ipamClient, fake.NewSimpleClientset())

	formValues := "csrf_token=" + csrfToken + "&project=other&ipaddr=1.2.3.4"
	req, err := http.NewRequest("POST", "/remove", strings.NewReader(formValues))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRemove)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusSeeOther {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusSeeOther)
	}
}

func TestHandleFIPList_IPAMError(t *testing.T) {
	mockIpamAPI := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ipam error", http.StatusInternalServerError)
	})
	defer mockIpamAPI.Close()

	ipamClient, err := ipam.NewClient(mockIpamAPI.URL, "test-client", nil)
	if err != nil {
		t.Fatal(err)
	}

	server, cookie, _ := newAuthenticatedServer(t, ipamClient, fake.NewSimpleClientset())

	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleFIPList)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusInternalServerError {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusInternalServerError)
	}
}

func TestHandleRelease_FormError(t *testing.T) {
	server, cookie, csrfToken := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	req, err := http.NewRequest("POST", "/release", strings.NewReader("csrf_token="+csrfToken+"&project=p"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRelease)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusBadRequest {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusBadRequest)
	}
}

func TestHandleRemove_FormError(t *testing.T) {
	server, cookie, csrfToken := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	req, err := http.NewRequest("POST", "/remove", strings.NewReader("csrf_token="+csrfToken))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(server.handleRemove)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusBadRequest {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusBadRequest)
	}
}

func TestHandleLogout_CSRFRejected(t *testing.T) {
	server, cookie, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	req, err := http.NewRequest("POST", "/logout", strings.NewReader("csrf_token=wrong"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rr := httptest.NewRecorder()
	server.handleLogout(rr, req)

	if status := rr.Code; status != http.StatusForbidden {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusForbidden)
	}
}

func TestMutatingHandlers_NonLeaderRejected(t *testing.T) {
	server, cookie, csrfToken := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())
	server.isLeader = func() bool { return false }

	body := "csrf_token=" + csrfToken + "&ipaddr=1.2.3.4&floatingippool=pool1&servicenamespace=ns&servicename=svc"
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"release", server.handleRelease},
		{"remove", server.handleRemove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/"+tc.name, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(cookie)

			rr := httptest.NewRecorder()
			tc.handler(rr, req)

			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("handler returned %d, want %d", rr.Code, http.StatusServiceUnavailable)
			}
			if got := rr.Header().Get("Retry-After"); got != "5" {
				t.Errorf("Retry-After = %q, want %q", got, "5")
			}
		})
	}
}

func TestServeHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- ServeHealth(ctx, port)
	}()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	for _, path := range []string{"/healthz", "/readyz"} {
		deadline := time.Now().Add(10 * time.Second)
		var resp *http.Response
		for time.Now().Before(deadline) {
			resp, err = http.Get(baseURL + path)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("health server did not start: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ServeHealth returned error on shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("health server did not shut down after context cancellation")
	}
}

func TestStart_BindErrorReturned(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	server, _, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Start(ctx, port)
	if err == nil {
		t.Fatal("expected a bind error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to listen") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHandleLogin_CookieHardened(t *testing.T) {
	server, _, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	req, err := http.NewRequest("POST", "/login", strings.NewReader("username=testuser&password=testpassword"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	server.handleLogin(rr, req)

	if status := rr.Code; status != http.StatusSeeOther {
		t.Fatalf("handler returned wrong status code: got %v want %v", status, http.StatusSeeOther)
	}

	setCookie := rr.Header().Get("Set-Cookie")
	for _, attr := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(setCookie, attr) {
			t.Errorf("session cookie missing %q: %s", attr, setCookie)
		}
	}
}

func TestStart_ServesAndShutsDown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	server, _, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start(ctx, port)
	}()

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/login"
	deadline := time.Now().Add(10 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /login returned %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned error on shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down after context cancellation")
	}
}

func TestWithAuth_DoesNotHoldSessionLock(t *testing.T) {
	server, cookie, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())

	handler := server.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if server.csrfTokenFor(r) == "" {
			t.Errorf("csrfTokenFor returned an empty token inside an authenticated handler")
		}
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rr, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler deadlocked on the session mutex")
	}
}

func TestPruneSessionsLocked_DropsExpired(t *testing.T) {
	server, _, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())
	now := time.Now()

	server.sessions["expired"] = session{expiresAt: now.Add(-time.Minute)}
	server.sessions["live"] = session{expiresAt: now.Add(time.Hour)}

	server.sessionsMutex.Lock()
	server.pruneSessionsLocked(now)
	server.sessionsMutex.Unlock()

	if _, ok := server.sessions["expired"]; ok {
		t.Error("expired session survived the sweep")
	}
	if _, ok := server.sessions["live"]; !ok {
		t.Error("live session was removed by the sweep")
	}
}

func TestPruneSessionsLocked_CapsMap(t *testing.T) {
	server, _, _ := newAuthenticatedServer(t, nil, fake.NewSimpleClientset())
	now := time.Now()

	server.sessionsMutex.Lock()
	for i := 0; i < maxSessions; i++ {
		server.sessions[strconv.Itoa(i)] = session{expiresAt: now.Add(time.Duration(i) * time.Second)}
	}
	server.pruneSessionsLocked(now)
	server.sessions["new"] = session{expiresAt: now.Add(time.Hour)}
	size := len(server.sessions)
	_, oldest := server.sessions["0"]
	server.sessionsMutex.Unlock()

	if size > maxSessions {
		t.Errorf("session map grew to %d, cap is %d", size, maxSessions)
	}
	if oldest {
		t.Error("session closest to expiry was not evicted")
	}
}
