package http

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/joeyloman/rancher-fip-lb-controller/pkg/ipam"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	sessionCookieName = "session_token"

	// sessionDuration bounds an authenticated session.
	sessionDuration = 24 * time.Hour

	// maxSessions caps the in-memory session map. Logins beyond the cap evict
	// the session closest to expiry, so unauthenticated requests cannot grow
	// memory without bound.
	maxSessions = 1024

	// sessionSweepInterval is how often expired sessions are dropped while the
	// server runs.
	sessionSweepInterval = 10 * time.Minute

	// releaseWaitTimeout bounds how long /release waits for the controller to
	// finish the Service deletion (finalizer removal) before reporting that
	// the release is still in progress.
	releaseWaitTimeout  = 30 * time.Second
	releasePollInterval = 500 * time.Millisecond

	// floatingIPAnnotation is the controller's record that a Service holds an
	// address. The UI only deletes Services carrying it.
	floatingIPAnnotation = "rancher.k8s.binbash.org/floatingip"

	// projectAnnotation records the project an address was allocated under.
	projectAnnotation = "rancher.k8s.binbash.org/floatingip-project"
)

// session is one authenticated login. The CSRF token is bound to the session
// so a state-changing form must originate from a page this server rendered.
type session struct {
	expiresAt time.Time
	csrfToken string
}

type Server struct {
	ipamClient      *ipam.Client
	clientset       kubernetes.Interface
	clientSecret    string
	cluster         string
	project         string
	floatingIPPools []string
	username        string
	password        string
	sessions        map[string]session
	sessionsMutex   sync.Mutex

	// csrfToken is used when authentication is disabled: there is no session
	// to bind a token to, but state-changing requests still need one.
	csrfToken string

	// isLeader reports whether this replica may mutate cluster state. The
	// management UI is served by every replica; mutating requests are refused
	// on a non-leader so they cannot run against a stale queue.
	isLeader func() bool

	// ready is closed once the listener is accepting connections.
	ready     chan struct{}
	readyOnce sync.Once
}

func NewServer(ipamClient *ipam.Client, clientset kubernetes.Interface, clientSecret, cluster, project string, floatingIPPools []string, username, password string, isLeader func() bool) (*Server, error) {
	csrfToken, err := newRandomToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate CSRF token: %w", err)
	}

	return &Server{
		ipamClient:      ipamClient,
		clientset:       clientset,
		clientSecret:    clientSecret,
		cluster:         cluster,
		project:         project,
		floatingIPPools: floatingIPPools,
		username:        username,
		password:        password,
		sessions:        make(map[string]session),
		csrfToken:       csrfToken,
		isLeader:        isLeader,
		ready:           make(chan struct{}),
	}, nil
}

// Ready is closed once the server is listening. The leader label is only
// applied after this, so the Service never routes to a port that is not open.
func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

// leaderReplica reports whether this replica may serve mutating requests.
// Without a checker (unit tests, embedded use) the server acts as leader.
func (s *Server) leaderReplica() bool {
	return s.isLeader == nil || s.isLeader()
}

func (s *Server) requireLeader(w http.ResponseWriter) bool {
	if s.leaderReplica() {
		return true
	}
	w.Header().Set("Retry-After", "5")
	http.Error(w, "this replica is not the leader; retry the request", http.StatusServiceUnavailable)
	return false
}

func newRandomToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(tokenBytes), nil
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authDisabled() {
			next(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			if err == http.ErrNoCookie {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		sessionToken := cookie.Value

		s.sessionsMutex.Lock()
		sess, ok := s.sessions[sessionToken]
		if !ok {
			s.sessionsMutex.Unlock()
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if time.Now().After(sess.expiresAt) {
			delete(s.sessions, sessionToken)
			s.sessionsMutex.Unlock()
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.sessionsMutex.Unlock()

		next(w, r)
	}
}

// pruneSessionsLocked drops expired sessions and evicts the sessions closest
// to expiry while the map is at its cap. The caller must hold sessionsMutex.
func (s *Server) pruneSessionsLocked(now time.Time) {
	for token, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, token)
		}
	}

	for len(s.sessions) >= maxSessions {
		var oldestToken string
		var oldestExpiry time.Time
		for token, sess := range s.sessions {
			if oldestToken == "" || sess.expiresAt.Before(oldestExpiry) {
				oldestToken, oldestExpiry = token, sess.expiresAt
			}
		}
		delete(s.sessions, oldestToken)
	}
}

// runSessionJanitor sweeps expired sessions until ctx is cancelled.
func (s *Server) runSessionJanitor(ctx context.Context) {
	ticker := time.NewTicker(sessionSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sessionsMutex.Lock()
			s.pruneSessionsLocked(now)
			s.sessionsMutex.Unlock()
		}
	}
}

func (s *Server) authDisabled() bool {
	return s.username == "" && s.password == ""
}

// csrfTokenFor returns the token bound to the caller's session, or the
// process-wide token when authentication is disabled.
func (s *Server) csrfTokenFor(r *http.Request) string {
	if s.authDisabled() {
		return s.csrfToken
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}

	s.sessionsMutex.Lock()
	defer s.sessionsMutex.Unlock()

	sess, ok := s.sessions[cookie.Value]
	if !ok || time.Now().After(sess.expiresAt) {
		return ""
	}
	return sess.csrfToken
}

// validCSRF compares the submitted token against the session's token in
// constant time. It is only meaningful after ParseForm.
func (s *Server) validCSRF(r *http.Request) bool {
	expected := s.csrfTokenFor(r)
	submitted := r.FormValue("csrf_token")
	if expected == "" || submitted == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(expected)) == 1
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		tmpl, err := template.New("login").Parse(loginTemplate)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		tmpl.Execute(w, nil)
	} else if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form", http.StatusBadRequest)
			return
		}
		username := r.FormValue("username")
		password := r.FormValue("password")

		if subtle.ConstantTimeCompare([]byte(username), []byte(s.username)) == 1 &&
			subtle.ConstantTimeCompare([]byte(password), []byte(s.password)) == 1 {
			sessionToken, err := newRandomToken()
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			csrfToken, err := newRandomToken()
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			expiresAt := time.Now().Add(sessionDuration)

			s.sessionsMutex.Lock()
			s.pruneSessionsLocked(time.Now())
			s.sessions[sessionToken] = session{expiresAt: expiresAt, csrfToken: csrfToken}
			s.sessionsMutex.Unlock()

			s.setSessionCookie(w, r, sessionToken, expiresAt, 0)

			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			tmpl, err := template.New("login").Parse(loginTemplate)
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			data := struct {
				Error string
			}{
				Error: "Unauthorized",
			}
			tmpl.Execute(w, data)
		}
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	sessionToken := cookie.Value

	s.sessionsMutex.Lock()
	delete(s.sessions, sessionToken)
	s.sessionsMutex.Unlock()

	s.setSessionCookie(w, r, "", time.Unix(1, 0), -1)

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Start serves the management UI until ctx is cancelled, then drains
// in-flight requests. It returns an error when the listener cannot be
// created; the caller must not treat that as fatal to the controller.
func (s *Server) Start(ctx context.Context, httpServerPort int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.withAuth(s.handleFIPList))
	mux.HandleFunc("/release", s.withAuth(s.handleRelease))
	mux.HandleFunc("/remove", s.withAuth(s.handleRemove))
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", httpServerPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// A /release may spend releaseWaitTimeout waiting for the controller
		// and then up to the IPAM client's 60s request timeout confirming the
		// release, so the write deadline must cover both.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", srv.Addr, err)
	}

	// Signal that the port is open; the leader label is only applied after
	// this so the Service never routes to a closed port.
	s.readyOnce.Do(func() { close(s.ready) })

	logrus.Infof("Starting HTTP server on :%d", httpServerPort)

	go s.runSessionJanitor(ctx)

	return serve(ctx, srv, listener)
}

// ServeHealth serves the liveness and readiness probes until ctx is
// cancelled. It runs on every replica, independently of whether the
// management UI could be configured, so a controller that is not part of a
// Rancher project still has working probes.
func ServeHealth(ctx context.Context, port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", srv.Addr, err)
	}

	logrus.Infof("Starting health server on :%d", port)

	return serve(ctx, srv, listener)
}

// serve runs srv on listener and shuts it down when ctx is cancelled.
func serve(ctx context.Context, srv *http.Server, listener net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("http server stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}
	return nil
}

func (s *Server) handleFIPList(w http.ResponseWriter, r *http.Request) {
	logrus.Info("Listing FIPs")

	fipList := &ipam.FIPListResponse{}

	for _, floatingIPPool := range s.floatingIPPools {
		fips, err := s.ipamClient.ListFIPs(r.Context(), s.clientSecret, s.cluster, s.project, floatingIPPool)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to list fips from pool %s: %s", floatingIPPool, err), http.StatusInternalServerError)
			return
		}
		if fips != nil {
			fipList.FloatingIPs = append(fipList.FloatingIPs, fips.FloatingIPs...)
		}
	}

	for i := range fipList.FloatingIPs {
		// logrus.Infof("REMOVE: FloatingIP from list [%+v]", fipList.FloatingIPs[i])
		if fipList.FloatingIPs[i].Cluster == "" {
			fipList.FloatingIPs[i].Cluster = "Unassigned"
		}
		if fipList.FloatingIPs[i].ServiceNamespace == "" {
			fipList.FloatingIPs[i].ServiceNamespace = "Unassigned"
		}
		if fipList.FloatingIPs[i].ServiceName == "" {
			fipList.FloatingIPs[i].ServiceName = "Unassigned"
		}
	}

	tmpl, err := template.New("fipList").Parse(fipListTemplate)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to parse template: %s", err), http.StatusInternalServerError)
		return
	}

	if err := tmpl.Execute(w, struct {
		*ipam.FIPListResponse
		CSRFToken string
	}{FIPListResponse: fipList, CSRFToken: s.csrfTokenFor(r)}); err != nil {
		http.Error(w, fmt.Sprintf("failed to execute template: %s", err), http.StatusInternalServerError)
		return
	}
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	logrus.Info("Releasing FIP")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.requireLeader(w) {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	if !s.validCSRF(r) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}

	floatingippool := r.FormValue("floatingippool")
	servicenamespace := r.FormValue("servicenamespace")
	servicename := r.FormValue("servicename")
	ipaddr := r.FormValue("ipaddr")
	floatingipgroup := r.FormValue("floatingipgroup")

	// Validate the form values. Project and cluster are deliberately not form
	// fields: they come from the configured Secret so a tampered form cannot
	// target another project or cluster.
	formValues := map[string]string{
		"floatingippool":   floatingippool,
		"servicenamespace": servicenamespace,
		"servicename":      servicename,
		"ipaddr":           ipaddr,
	}

	for key, value := range formValues {
		if value == "" {
			http.Error(w, fmt.Sprintf("%s is a required field", key), http.StatusBadRequest)
			return
		}

		if len(value) > 253 {
			http.Error(w, fmt.Sprintf("%s is too long, maximum length is 253 characters", key), http.StatusBadRequest)
			return
		}
	}

	if net.ParseIP(ipaddr) == nil {
		http.Error(w, "invalid ip address format for ipaddr", http.StatusBadRequest)
		return
	}

	logrus.Infof("Releasing FIP %s from project %s", ipaddr, s.project)

	// Bound the whole operation: the Service deletion wait must finish before
	// the server's WriteTimeout.
	ctx, cancel := context.WithTimeout(r.Context(), releaseWaitTimeout+10*time.Second)
	defer cancel()

	// First try to delete the service if it exists, so it will release the FIP
	// automatically. Only delete Services this controller manages: the form
	// must not be able to delete arbitrary Services with the controller's
	// cluster-wide credentials.
	svc, err := s.clientset.CoreV1().Services(servicenamespace).Get(ctx, servicename, metav1.GetOptions{})
	if err == nil {
		if svc.Annotations[floatingIPAnnotation] != ipaddr {
			http.Error(w, fmt.Sprintf("service %s/%s is not managed by this controller or does not hold %s", servicenamespace, servicename, ipaddr), http.StatusConflict)
			return
		}
		if allocatedProject := svc.Annotations[projectAnnotation]; allocatedProject != "" && allocatedProject != s.project {
			http.Error(w, fmt.Sprintf("service %s/%s belongs to project %s, not %s", servicenamespace, servicename, allocatedProject, s.project), http.StatusConflict)
			return
		}

		logrus.Infof("Service %s/%s exists, deleting it to release FIP", servicenamespace, servicename)
		err = s.clientset.CoreV1().Services(servicenamespace).Delete(ctx, servicename, metav1.DeleteOptions{})
		if err != nil {
			logrus.Errorf("failed to delete service %s/%s: %s", servicenamespace, servicename, err)
			http.Error(w, fmt.Sprintf("failed to delete service %s/%s: %s", servicenamespace, servicename, err), http.StatusInternalServerError)
			return
		}

		// The controller removes the finalizer only after the IPAM release
		// succeeds, so wait for the Service to disappear instead
		// of assuming a one-second sleep is enough.
		if err := s.waitForServiceGone(ctx, servicenamespace, servicename); err != nil {
			logrus.Errorf("service %s/%s still terminating: %s", servicenamespace, servicename, err)
			http.Error(w, fmt.Sprintf("service %s/%s deletion was requested, but the controller has not finished releasing %s within %s; check the controller logs and retry",
				servicenamespace, servicename, ipaddr, releaseWaitTimeout), http.StatusGatewayTimeout)
			return
		}

		// A Service without the finalizer disappears without the controller
		// ever releasing its address, so confirm the release explicitly.
		// Release is idempotent (404 is treated as success), which also covers
		// the normal finalizer path having already released it.
		if err := s.ipamClient.ReleaseFIP(ctx, s.clientSecret, s.cluster, s.project, floatingippool, servicenamespace, servicename, ipaddr, floatingipgroup); err != nil {
			logrus.Errorf("failed to release fip after deleting service %s/%s: %s", servicenamespace, servicename, err)
			http.Error(w, fmt.Sprintf("service %s/%s was deleted, but releasing %s failed: %s; retry the release",
				servicenamespace, servicename, ipaddr, err), http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	} else if !errors.IsNotFound(err) {
		logrus.Errorf("failed to check if service %s/%s exists: %s", servicenamespace, servicename, err)
		http.Error(w, fmt.Sprintf("failed to check if service %s/%s exists: %s", servicenamespace, servicename, err), http.StatusInternalServerError)
		return
	}

	logrus.Infof("Service %s/%s does not exist, releasing FIP", servicenamespace, servicename)
	if err := s.ipamClient.ReleaseFIP(ctx, s.clientSecret, s.cluster, s.project, floatingippool, servicenamespace, servicename, ipaddr, floatingipgroup); err != nil {
		logrus.Errorf("failed to release fip: %s", err)
		http.Error(w, fmt.Sprintf("failed to release fip: %s", err), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) waitForServiceGone(ctx context.Context, namespace, name string) error {
	return wait.PollUntilContextTimeout(ctx, releasePollInterval, releaseWaitTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := s.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	logrus.Info("Removing FIP")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.requireLeader(w) {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	if !s.validCSRF(r) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}

	// The project comes from the configured Secret, never the form.
	ipaddr := r.FormValue("ipaddr")

	if ipaddr == "" {
		http.Error(w, "ipaddr is a required field", http.StatusBadRequest)
		return
	}

	if len(ipaddr) > 253 {
		http.Error(w, "ipaddr is too long, maximum length is 253 characters", http.StatusBadRequest)
		return
	}

	if net.ParseIP(ipaddr) == nil {
		http.Error(w, "invalid ip address format for ipaddr", http.StatusBadRequest)
		return
	}

	logrus.Infof("Removing FIP %s from project %s", ipaddr, s.project)

	if err := s.ipamClient.DeleteFIP(r.Context(), s.clientSecret, s.project, ipaddr); err != nil {
		logrus.Errorf("failed to remove fip: %s", err)
		http.Error(w, fmt.Sprintf("failed to remove fip: %s", err), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

const fipListTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Rancher FIP LB Controller - Floating IP List</title>
	<style>
		body { font-family: sans-serif; }
		table { border-collapse: collapse; }
		th, td { border: 1px solid #dddddd; text-align: left; padding: 8px; }
		tr:nth-child(even) { background-color: #f2f2f2; }
		button { margin-right: 5px; }
	</style>
</head>
<body>
    <h1>Rancher FIP LB Controller - Floating IPs</h1>
	<form action="/logout" method="post" style="margin-bottom: 1em;">
        <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
        <button type="submit">Logout</button>
    </form>
    <table border="1">
        <tr>
            <th>Project</th>
            <th>Cluster</th>
            <th>Service Namespace</th>
            <th>Service Name</th>
            <th>IP Address</th>
            <th>FloatingIP Group</th>
			<th>Actions</th>
        </tr>
        {{range .FloatingIPs}}
        <tr>
            <td>{{.Project}}</td>
            <td>{{.Cluster}}</td>
            <td>{{.ServiceNamespace}}</td>
            <td>{{.ServiceName}}</td>
            <td>{{.IPAddress}}</td>
            <td>{{.FloatingIPGroup}}</td>
            <td>
                <form action="/release" method="post" style="display:inline;">
                    <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
    	            <input type="hidden" name="floatingippool" value="{{.FloatingIPPool}}">
    	            <input type="hidden" name="servicenamespace" value="{{.ServiceNamespace}}">
    	            <input type="hidden" name="servicename" value="{{.ServiceName}}">
                    <input type="hidden" name="ipaddr" value="{{.IPAddress}}">
    	            <input type="hidden" name="floatingipgroup" value="{{.FloatingIPGroup}}">
					<button type="submit" {{if eq .Cluster "Unassigned"}}disabled{{end}} onclick="return confirm('Are you sure you want to release FloatingIP {{.IPAddress}} from cluster {{.Cluster}}?');">Release from cluster</button>
                </form>
                <form action="/remove" method="post" style="display:inline;">
                    <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
                    <input type="hidden" name="ipaddr" value="{{.IPAddress}}">
                    <button type="submit" {{if ne .Cluster "Unassigned"}}disabled{{end}} onclick="return confirm('Are you sure you want to remove FloatingIP {{.IPAddress}} from project {{.Project}}?');">Remove from project</button>
                </form>
            </td>
        </tr>
        {{end}}
    </table>
</body>
</html>
`

const loginTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Rancher FIP LB Controller Login</title>
	<style>
		body { font-family: sans-serif; }
	</style>
</head>
<body>
    <h1>Rancher FIP LB Controller Login</h1>
	{{if .Error}}
		<p style="color: red;">{{.Error}}</p>
	{{end}}
    <form method="post" action="/login">
        <div>
            <label for="username">Username:</label>
            <input type="text" id="username" name="username">
        </div>
        <div style="margin-top: 0.5em;">
            <label for="password">Password:</label>
            <input type="password" id="password" name="password">
        </div>
        <div style="margin-top: 1em;">
            <input type="submit" value="Login">
        </div>
    </form>
</body>
</html>
`
