package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	rfmv2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestValidateFloatingIP(t *testing.T) {
	ipAddr := "192.168.1.100"
	fipPool := &rfmv2.FloatingIPPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rancher.k8s.binbash.org/v1beta2",
			Kind:       "FloatingIPPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pool",
		},
		Spec: rfmv2.FloatingIPPoolSpec{
			IPConfig: &rfmv2.IPConfig{
				Subnet: "192.168.1.0/24",
				Pool: rfmv2.Pool{
					Start:   "192.168.1.10",
					End:     "192.168.1.200",
					Exclude: []string{"192.168.1.101"},
				},
			},
		},
		Status: rfmv2.FloatingIPPoolStatus{
			Allocated: map[string]string{
				"192.168.1.102": "default/another-fip",
			},
			Available: 1,
		},
	}
	plbc := &rfmv2.FloatingIPProjectQuota{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rancher.k8s.binbash.org/v1beta2",
			Kind:       "FloatingIPProjectQuota",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
		},
		Spec: rfmv2.FloatingIPProjectQuotaSpec{
			FloatingIPQuota: map[string]int{
				"test-pool": 1,
			},
		},
		Status: rfmv2.FloatingIPProjectQuotaStatus{
			FloatingIPs: map[string]*rfmv2.FipInfo{
				"test-pool": {
					Used: 0,
				},
			},
		},
	}
	fip := &rfmv2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-fip",
			Namespace: "default",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "test-project",
			},
		},
		Spec: rfmv2.FloatingIPSpec{
			FloatingIPPool: "test-pool",
		},
	}

	testCases := []struct {
		name            string
		fip             *rfmv2.FloatingIP
		existingPools   []runtime.Object
		existingPLBCs   []runtime.Object
		existingFIPs    []runtime.Object
		expectedAllowed bool
		expectedMessage string
	}{
		{
			name:            "pool does not exist",
			fip:             fip,
			existingPools:   []runtime.Object{},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "the specified floatingippool test-pool does not exist",
		},
		{
			name: "invalid ip address",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					IPAddr:         new(string), // empty string will be invalid
				},
			},
			existingPools:   []runtime.Object{fipPool},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "invalid IP address format: ",
		},
		{
			name: "ip not in subnet",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					IPAddr:         func() *string { s := "192.168.2.1"; return &s }(),
				},
			},
			existingPools:   []runtime.Object{fipPool},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "requested IP 192.168.2.1 is not in the subnet range 192.168.1.0/24",
		},
		{
			name: "ip in exclude list",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					IPAddr:         func() *string { s := "192.168.1.101"; return &s }(),
				},
			},
			existingPools:   []runtime.Object{fipPool},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "requested IP 192.168.1.101 is in the exclude list",
		},
		{
			name: "ip already allocated",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					IPAddr:         func() *string { s := "192.168.1.102"; return &s }(),
				},
			},
			existingPools:   []runtime.Object{fipPool},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "requested IP 192.168.1.102 is already allocated",
		},
		{
			name: "pool is full",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
				},
			},
			existingPools: []runtime.Object{
				&rfmv2.FloatingIPPool{
					TypeMeta:   fipPool.TypeMeta,
					ObjectMeta: fipPool.ObjectMeta,
					Spec:       fipPool.Spec,
					Status: rfmv2.FloatingIPPoolStatus{
						Available: 0,
					},
				},
			},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "no available IPs in floatingippool test-pool",
		},
		{
			name:          "no quota defined",
			fip:           fip,
			existingPools: []runtime.Object{fipPool},
			existingPLBCs: []runtime.Object{
				&rfmv2.FloatingIPProjectQuota{
					TypeMeta:   plbc.TypeMeta,
					ObjectMeta: plbc.ObjectMeta,
					Spec:       rfmv2.FloatingIPProjectQuotaSpec{}, // no quota
				},
			},
			expectedAllowed: false,
			expectedMessage: "no quota defined for floatingippool test-pool in project test-project",
		},
		{
			name:          "quota exceeded",
			fip:           fip,
			existingPools: []runtime.Object{fipPool},
			existingPLBCs: []runtime.Object{
				&rfmv2.FloatingIPProjectQuota{
					TypeMeta:   plbc.TypeMeta,
					ObjectMeta: plbc.ObjectMeta,
					Spec:       plbc.Spec,
					Status: rfmv2.FloatingIPProjectQuotaStatus{
						FloatingIPs: map[string]*rfmv2.FipInfo{
							"test-pool": {
								Used: 1, // quota is 1
							},
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "quota exceeded for floatingippool test-pool in project test-project. Quota: 1, Used: 1",
		},
		{
			name: "valid request",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					IPAddr:         &ipAddr,
				},
			},
			existingPools:   []runtime.Object{fipPool},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: true,
		},
		{
			name: "deletion of an out-of-range fip is always allowed",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-fip",
					Namespace:         "default",
					Labels:            fip.Labels,
					DeletionTimestamp: &metav1.Time{},
					Finalizers:        []string{"floatingip-cleanup"},
				},
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					// Outside subnet, outside range, absent from status:
					// every re-validation would deny this update were it
					// not a deletion.
					IPAddr: func() *string { s := "192.168.9.9"; return &s }(),
				},
			},
			existingPools:   []runtime.Object{fipPool},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ar := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UID: "test-uid",
				},
			}
			unstructuredPools, _ := getUnstructuredList(tc.existingPools)
			unstructuredPLBCs, _ := getUnstructuredList(tc.existingPLBCs)
			unstructuredFIPs, _ := getUnstructuredList(tc.existingFIPs)

			allObjects := append(unstructuredPools, unstructuredPLBCs...)
			allObjects = append(allObjects, unstructuredFIPs...)
			dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme(), allObjects...)

			response := validateFloatingIP(context.Background(), dynamicClient, ar, tc.fip, nil, nil)

			assert.Equal(t, tc.expectedAllowed, response.Allowed)
			if !tc.expectedAllowed {
				assert.Equal(t, tc.expectedMessage, response.Result.Message)
			}
		})
	}
}

func TestValidateFloatingIPPool(t *testing.T) {
	validFipPool := &rfmv2.FloatingIPPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rancher.k8s.binbash.org/v1beta2",
			Kind:       "FloatingIPPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pool",
		},
		Spec: rfmv2.FloatingIPPoolSpec{
			IPConfig: &rfmv2.IPConfig{
				Subnet: "192.168.1.0/24",
				Pool: rfmv2.Pool{
					Start:   "192.168.1.10",
					End:     "192.168.1.20",
					Exclude: []string{"192.168.1.15"},
				},
			},
		},
	}

	testCases := []struct {
		name            string
		fipPool         *rfmv2.FloatingIPPool
		expectedAllowed bool
		expectedMessage string
	}{
		{
			name: "invalid subnet format",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "invalid-subnet",
						Pool:   validFipPool.Spec.IPConfig.Pool,
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "invalid subnet format: invalid-subnet",
		},
		{
			name: "invalid start IP address format",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "invalid-ip",
							End:     validFipPool.Spec.IPConfig.Pool.End,
							Exclude: validFipPool.Spec.IPConfig.Pool.Exclude,
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "invalid start IP address format: invalid-ip",
		},
		{
			name: "start IP not within subnet",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.2.10",
							End:     validFipPool.Spec.IPConfig.Pool.End,
							Exclude: validFipPool.Spec.IPConfig.Pool.Exclude,
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "start IP address 192.168.2.10 is not within the subnet 192.168.1.0/24",
		},
		{
			name: "invalid end IP address format",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   validFipPool.Spec.IPConfig.Pool.Start,
							End:     "invalid-ip",
							Exclude: validFipPool.Spec.IPConfig.Pool.Exclude,
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "invalid end IP address format: invalid-ip",
		},
		{
			name: "end IP not within subnet",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   validFipPool.Spec.IPConfig.Pool.Start,
							End:     "192.168.2.20",
							Exclude: validFipPool.Spec.IPConfig.Pool.Exclude,
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "end IP address 192.168.2.20 is not within the subnet 192.168.1.0/24",
		},
		{
			name: "start IP greater than end IP",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.1.20",
							End:     "192.168.1.10",
							Exclude: validFipPool.Spec.IPConfig.Pool.Exclude,
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "start IP address 192.168.1.20 must be less than or equal to end IP address 192.168.1.10",
		},
		{
			name: "pool range exceeds maximum size",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "10.0.0.0/8",
						Pool: rfmv2.Pool{
							Start:   "10.0.0.1",
							End:     "10.1.0.1",
							Exclude: []string{},
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: fmt.Sprintf("pool range 10.0.0.1-10.1.0.1 spans 65537 addresses, exceeding the maximum of %d", ipam.MaxSubnetRangeSize),
		},
		{
			name: "pool range at maximum size",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "10.0.0.0/8",
						Pool: rfmv2.Pool{
							Start:   "10.0.0.1",
							End:     "10.0.255.254",
							Exclude: []string{},
						},
					},
				},
			},
			expectedAllowed: true,
		},
		{
			name: "invalid excluded IP address format",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   validFipPool.Spec.IPConfig.Pool.Start,
							End:     validFipPool.Spec.IPConfig.Pool.End,
							Exclude: []string{"invalid-ip"},
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "invalid excluded IP address format: invalid-ip",
		},
		{
			name: "excluded IP not within subnet",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   validFipPool.Spec.IPConfig.Pool.Start,
							End:     validFipPool.Spec.IPConfig.Pool.End,
							Exclude: []string{"192.168.2.15"},
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "excluded IP address 192.168.2.15 is not within the subnet 192.168.1.0/24",
		},
		{
			name: "excluded IP before pool start",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.1.10",
							End:     "192.168.1.20",
							Exclude: []string{"192.168.1.5"},
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "excluded IP address 192.168.1.5 is not within the pool range [192.168.1.10, 192.168.1.20]",
		},
		{
			name: "excluded IP after pool end",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.1.10",
							End:     "192.168.1.20",
							Exclude: []string{"192.168.1.25"},
						},
					},
				},
			},
			expectedAllowed: false,
			expectedMessage: "excluded IP address 192.168.1.25 is not within the pool range [192.168.1.10, 192.168.1.20]",
		},
		{
			name:            "valid request",
			fipPool:         validFipPool,
			expectedAllowed: true,
		},
		{
			name: "valid request with multiple excluded IPs",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.1.10",
							End:     "192.168.1.20",
							Exclude: []string{"192.168.1.15", "192.168.1.18"},
						},
					},
				},
			},
			expectedAllowed: true,
		},
		{
			name: "valid request with no excluded IPs",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.1.10",
							End:     "192.168.1.20",
							Exclude: []string{},
						},
					},
				},
			},
			expectedAllowed: true,
		},
		{
			name: "valid request with start equal to end",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec: rfmv2.FloatingIPPoolSpec{
					IPConfig: &rfmv2.IPConfig{
						Subnet: "192.168.1.0/24",
						Pool: rfmv2.Pool{
							Start:   "192.168.1.15",
							End:     "192.168.1.15",
							Exclude: []string{},
						},
					},
				},
			},
			expectedAllowed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ar := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UID: "test-uid",
				},
			}

			response := validateFloatingIPPool(context.Background(), ar, tc.fipPool)

			assert.Equal(t, tc.expectedAllowed, response.Allowed)
			if !tc.expectedAllowed {
				assert.Equal(t, tc.expectedMessage, response.Result.Message)
			}
		})
	}
}

func getUnstructuredList(objects []runtime.Object) ([]runtime.Object, error) {
	unstructuredList := []runtime.Object{}
	for _, obj := range objects {
		unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		unstructuredList = append(unstructuredList, &unstructured.Unstructured{Object: unstructuredMap})
	}
	return unstructuredList, nil
}

// TestStopWithoutServerIsNil pins the nil-safety half of REPORT.md 2.5.
func TestStopWithoutServerIsNil(t *testing.T) {
	h := &Handler{ctx: context.Background()}
	require.NoError(t, h.Stop())
}

// TestStopIsBoundedByTimeout pins REPORT.md 2.5: Stop used to be bounded
// only by the app context (SIGTERM), so a wedged in-flight request could
// block a renewal/rebind indefinitely. Stop must now return with a
// deadline error within shutdownTimeout even while a handler hangs.
func TestStopIsBoundedByTimeout(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 1 * time.Second
	t.Cleanup(func() { shutdownTimeout = old })

	blocked := make(chan struct{})
	defer close(blocked)

	srv := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-blocked
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go srv.Serve(ln)

	h := &Handler{ctx: context.Background()}
	h.mu.Lock()
	h.httpServer = srv
	h.mu.Unlock()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /hung HTTP/1.1\r\nHost: localhost\r\n\r\n")
	require.NoError(t, err)
	// let the request reach the (blocking) handler
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	err = h.Stop()
	elapsed := time.Since(start)

	require.Error(t, err, "Shutdown of a server with a wedged handler must report the deadline")
	assert.GreaterOrEqual(t, elapsed, shutdownTimeout)
	assert.Less(t, elapsed, shutdownTimeout+3*time.Second, "Stop must be bounded by shutdownTimeout, not by the wedged handler")
}

func newServingPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "rancher-fip-manager-webhook.rancher-fip-manager.svc"},
		DNSNames:  []string{"rancher-fip-manager-webhook.rancher-fip-manager.svc"},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

// TestServesRotatedCertificateInPlace pins the central REPORT.md 2.4 claim:
// renewal swaps the served certificate on the LIVE listener — pair A is
// served, then pair B is pushed, and the very next handshake serves B
// without the listener ever being stopped (the old path ran Stop →
// sleep(2s) → restart per renewal, a recurring write-blackout window under
// failurePolicy Fail).
func TestServesRotatedCertificateInPlace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h := &Handler{ctx: context.Background()}
	go h.Run()
	require.Eventually(t, h.IsServing, 5*time.Second, 10*time.Millisecond, "listener must bind even before any material exists")

	dial := func() (*x509.Certificate, error) {
		conn, err := tls.Dial("tcp", "127.0.0.1:8443", &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return nil, err
		}
		defer conn.Close()

		return conn.ConnectionState().PeerCertificates[0], nil
	}

	// No material anywhere yet: the handshake must fail closed, /readyz
	// must be red, and the process must still be up to recover.
	_, err := dial()
	require.Error(t, err, "handshake without any serving material must fail")

	certPEM, keyPEM := newServingPEM(t)
	pairA, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	h.SetCertificate(&pairA)

	peerA, err := dial()
	require.NoError(t, err)

	certPEM, keyPEM = newServingPEM(t)
	pairB, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	h.SetCertificate(&pairB)

	peerB, err := dial()
	require.NoError(t, err)
	require.NotEqual(t, peerA.Raw, peerB.Raw, "the pushed material must be served on the next handshake without a restart")

	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Get("https://127.0.0.1:8443/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "readyz must be green while serving rotated material")

	assert.True(t, h.IsServing(), "rotation must never tear down the listener")
	require.NoError(t, h.Stop())
}

// TestReadyzReportsServingMaterial pins the readiness half of REPORT.md
// 5.1: /readyz must be red while no TLS material can produce a successful
// handshake (endpoint shed under Fail policy), and green after either a
// pushed (renewed) certificate or with the on-disk pair.
func TestReadyzReportsServingMaterial(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	h := &Handler{ctx: context.Background()}
	rec := httptest.NewRecorder()
	h.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "no material anywhere must not advertise Ready")

	// pushed material (post-renewal state)
	h.SetCertificate(&tls.Certificate{Certificate: [][]byte{{0x30, 0x00}}})
	rec = httptest.NewRecorder()
	h.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	// file fallback: fresh handler, valid on-disk pair, nothing pushed
	certPEM, keyPEM := newServingPEM(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0600))
	h2 := &Handler{ctx: context.Background()}
	rec = httptest.NewRecorder()
	h2.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	// broken on-disk key must stay red
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), []byte("not a key"), 0600))
	h3 := &Handler{ctx: context.Background()}
	rec = httptest.NewRecorder()
	h3.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
