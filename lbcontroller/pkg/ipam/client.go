package ipam

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client is a client for the external IPAM API.
type Client struct {
	httpClient *http.Client
	apiURL     string
	clientID   string
}

// FIPListResponse defines the structure for the response to a floating IP list request.
type FIPListResponse struct {
	// ClientSecret is an extra secret for authenticating client requests.
	ClientSecret string `json:"clientsecret"`
	// Cluster is the target cluster.
	Cluster string `json:"cluster"`
	// Project is the project for which floating IPs are listed.
	Project string `json:"project"`
	// FloatingIPs is the list of floating IPs.
	FloatingIPs []FloatingIP `json:"floatingips"`
}

// FloatingIP defines the structure for a single floating IP in the list response.
type FloatingIP struct {
	// Project is the project that owns the floating IP.
	Project string `json:"project"`
	// Cluster is the cluster where the floating IP is allocated.
	Cluster string `json:"cluster"`
	// FloatingIPPool is the pool where the floating IP is allocated.
	FloatingIPPool string `json:"floatingippool"`
	// ServiceNamespace is the namespace of the service to expose.
	ServiceNamespace string `json:"servicenamespace"`
	// ServiceName is the name of the service to expose.
	ServiceName string `json:"servicename"`
	// IPAddress is the floating IP address.
	IPAddress string `json:"ipaddr"`
	// FloatingIPGroup is the group identifier for shared IP requests.
	FloatingIPGroup string `json:"floatingipgroup,omitempty"`
}

// DeleteFIPRequest defines the structure for the request to delete a floating IP.
type DeleteFIPRequest struct {
	ClientSecret string `json:"clientsecret"`
	Project      string `json:"project"`
	IPAddress    string `json:"ipaddr"`
}

// FIPListRequest defines the structure for the fip-list request.
type FIPListRequest struct {
	ClientSecret string `json:"clientsecret"`
}

const (
	// tokenTimeout bounds authentication calls, which are cheap and must not
	// hold a reconcile worker for long.
	tokenTimeout = 10 * time.Second

	// requestTimeout must exceed the IPAM manager's worst-case request
	// duration (MaxRequestDuration: 30s allocation wait + 10s cleanup).
	// Aborting earlier cancels the manager's allocation watch mid-write,
	// which triggers retry churn and can leave a second FloatingIP behind.
	requestTimeout = 60 * time.Second
)

// CAStore holds the CA bundle used to verify the IPAM manager. Update replaces
// the bundle atomically and is safe to call while requests are in flight, so a
// rotated cacerts Secret takes effect on the next TLS handshake without
// restarting the controller.
type CAStore struct {
	pool atomic.Pointer[x509.CertPool]
}

// NewCAStore parses a PEM bundle. Empty data means the system roots are used.
// An invalid bundle is rejected instead of silently leaving an empty pool that
// only fails later during the TLS handshake.
func NewCAStore(caCertData []byte) (*CAStore, error) {
	store := &CAStore{}
	if err := store.Update(caCertData); err != nil {
		return nil, err
	}
	return store, nil
}

// Update replaces the CA bundle. On error the previous bundle stays in effect.
func (s *CAStore) Update(caCertData []byte) error {
	if len(caCertData) == 0 {
		s.pool.Store(nil)
		return nil
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertData) {
		return fmt.Errorf("CA bundle contains no valid PEM certificate")
	}
	s.pool.Store(pool)
	return nil
}

// Pool returns the current bundle, or nil when the system roots are used.
func (s *CAStore) Pool() *x509.CertPool {
	if s == nil {
		return nil
	}
	return s.pool.Load()
}

// NewClient creates a new IPAM client that verifies the manager with
// caCertData.
func NewClient(apiURL string, clientID string, caCertData []byte) (*Client, error) {
	store, err := NewCAStore(caCertData)
	if err != nil {
		return nil, err
	}
	return NewClientWithCA(apiURL, clientID, store)
}

// NewClientWithCA creates a client that reads its trusted CA bundle from store
// on every TLS handshake. A nil store means the system roots.
func NewClientWithCA(apiURL string, clientID string, store *CAStore) (*Client, error) {
	if apiURL == "" {
		return nil, fmt.Errorf("IPAM API URL is not configured")
	}

	// The trusted pool is looked up per connection rather than baked into a
	// tls.Config, so CA rotation needs no client rebuild. Per-call deadlines
	// are applied in do; the client itself has no global timeout so token and
	// allocation requests can use different budgets.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &tls.Dialer{Config: &tls.Config{RootCAs: store.Pool()}}
			return dialer.DialContext(ctx, network, addr)
		},
	}

	return &Client{
		httpClient: &http.Client{Transport: transport},
		apiURL:     apiURL,
		clientID:   clientID,
	}, nil
}

// do issues req with a per-call deadline. The client is copied so the shared
// Transport (and its connection pool) is reused while the timeout applies only
// to this request, including reading the response body.
func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, error) {
	client := *c.httpClient
	client.Timeout = timeout
	return client.Do(req)
}

// getToken gets a JWT from the IPAM API
func (c *Client) getToken(ctx context.Context) (string, error) {
	reqBody := map[string]string{
		"clientID": c.clientID,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("request marshal error: %w", err)
	}

	url := fmt.Sprintf("%s/auth/token", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req, tokenTimeout)
	if err != nil {
		return "", fmt.Errorf("client error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status code %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("response error: %w", err)
	}

	var tokenResponse struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", fmt.Errorf("response unmarshal error: %w", err)
	}

	return tokenResponse.Token, nil
}

// RequestFIP requests a floating IP address from the IPAM API.
func (c *Client) RequestFIP(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get token for FIP request: %w", err)
	}

	reqBody := map[string]string{
		"clientSecret":     clientSecret,
		"cluster":          cluster,
		"project":          project,
		"floatingippool":   floatingIPPool,
		"servicenamespace": serviceNamespace,
		"servicename":      serviceName,
		"ipaddr":           ipaddr,
	}
	if floatingIPGroup != "" {
		reqBody["floatingipgroup"] = floatingIPGroup
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to marshal FIP request body: %w", err)
	}

	url := fmt.Sprintf("%s/fip/request", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create FIP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.do(req, requestTimeout)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to request FIP: %w", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to read FIP response body: %w", err)
	}

	var fipResponse struct {
		ClientSecret     string `json:"clientSecret"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		Cluster          string `json:"cluster"`
		Project          string `json:"project"`
		FloatingIPPool   string `json:"floatingippool"`
		ServiceNamespace string `json:"servicenamespace"`
		ServiceName      string `json:"servicename"`
		IPAddress        string `json:"ipaddr"`
		Subnet           string `json:"subnet"`
		FloatingIPGroup  string `json:"floatingipgroup,omitempty"`
		SharedKey        string `json:"sharedkey,omitempty"`
	}
	if err := json.Unmarshal(body, &fipResponse); err != nil {
		return "", "", "", fmt.Errorf("failed to unmarshal FIP response: %w", err)
	}

	// check for errors
	if resp.StatusCode != http.StatusOK {
		if strings.Contains(string(body), "quota exceeded") {
			return "", "", "", fmt.Errorf("failed to request FIP: quota exceeded")
		}
		return "", "", "", fmt.Errorf("failed to request FIP: status code %d (%s)", resp.StatusCode, string(body))
	}

	// do some validation checks
	if fipResponse.ClientSecret != clientSecret {
		return "", "", "", fmt.Errorf("failed to request FIP: client secret mismatch")
	}
	if fipResponse.Status != "approved" {
		return "", "", "", fmt.Errorf("failed to request FIP: status %s", fipResponse.Status)
	}
	if fipResponse.Cluster != cluster {
		return "", "", "", fmt.Errorf("failed to request FIP: cluster mismatch")
	}
	if fipResponse.Project != project {
		return "", "", "", fmt.Errorf("failed to request FIP: project mismatch")
	}
	if fipResponse.FloatingIPPool != floatingIPPool {
		return "", "", "", fmt.Errorf("failed to request FIP: floating IP pool mismatch")
	}
	if fipResponse.ServiceNamespace != serviceNamespace {
		return "", "", "", fmt.Errorf("failed to request FIP: service namespace mismatch")
	}
	if fipResponse.ServiceName != serviceName {
		return "", "", "", fmt.Errorf("failed to request FIP: service name mismatch")
	}
	if fipResponse.IPAddress == "" {
		return "", "", "", fmt.Errorf("failed to request FIP: IP address is empty")
	}
	if fipResponse.FloatingIPGroup != floatingIPGroup {
		return "", "", "", fmt.Errorf("failed to request FIP: floatingip-group name mismatch")
	}

	return fipResponse.IPAddress, fipResponse.Subnet, fipResponse.SharedKey, nil
}

// ReleaseFIP releases a floating IP address via the IPAM API.
func (c *Client) ReleaseFIP(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get token for FIP release: %w", err)
	}

	reqBody := map[string]string{
		"clientSecret":     clientSecret,
		"cluster":          cluster,
		"project":          project,
		"floatingippool":   floatingIPPool,
		"servicenamespace": serviceNamespace,
		"servicename":      serviceName,
		"ipaddr":           ipaddr,
	}
	if floatingIPGroup != "" {
		reqBody["floatingipgroup"] = floatingIPGroup
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal FIP release request body: %w", err)
	}

	url := fmt.Sprintf("%s/fip/release", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create FIP release request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.do(req, requestTimeout)
	if err != nil {
		return fmt.Errorf("failed to release FIP: %w", err)
	}
	defer resp.Body.Close()

	// Release means "ensure absent", not "delete". Once the manager has
	// processed a release it drops the service and cluster labels, so a retry
	// (after a crash, a failed annotation update, or a failed finalizer
	// update) lists no matching FloatingIP and answers 404. Treating that as
	// an error would leave the Service stuck in Terminating forever.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to release FIP: status code %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read FIP release response body: %w", err)
	}

	var fipResponse struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &fipResponse); err != nil {
		return fmt.Errorf("failed to unmarshal FIP release response: %w", err)
	}

	if fipResponse.Status != "released" {
		return fmt.Errorf("failed to release FIP: status %s", fipResponse.Status)
	}

	return nil
}

// ListFIPs gets a list of all floating IPs from the IPAM API.
func (c *Client) ListFIPs(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*FIPListResponse, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get token for FIP list request: %w", err)
	}

	// Get a new IP address from the IPAM API.
	reqBody := map[string]string{
		"clientSecret":   clientSecret,
		"cluster":        cluster,
		"project":        project,
		"floatingippool": floatingIPPool,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/fip/list", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create FIP list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.do(req, requestTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to get FloatingIP list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get FloatingIP list: status code %d", resp.StatusCode)
	}
	var fipListResponse FIPListResponse
	if err := json.NewDecoder(resp.Body).Decode(&fipListResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &fipListResponse, nil
}

// DeleteFIP deletes a floating IP from the IPAM API.
// Note: The user requested a DELETE http method, but to be consistent with the other functions
// in this client which send a body, we are using POST here. The endpoint is assumed to be /fip-delete.
func (c *Client) DeleteFIP(ctx context.Context, clientSecret, project, ipaddr string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get token for FIP delete request: %w", err)
	}

	reqBody := map[string]string{
		"clientSecret": clientSecret,
		"project":      project,
		"ipaddr":       ipaddr,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/fip/delete", c.apiURL)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create FIP delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.do(req, requestTimeout)
	if err != nil {
		return fmt.Errorf("failed to delete ip address: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to delete ip address: status code %d", resp.StatusCode)
	}
	return nil
}
