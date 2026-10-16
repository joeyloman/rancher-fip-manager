package service

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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
)

// clearAdmissionState wipes the package-global in-process admission
// reservations between tests and subtests: an allow in one test otherwise
// leaks a quota/IP reservation into the next test's decision for the TTL.
func clearAdmissionState() {
	ipReservations.Range(func(key, _ any) bool {
		ipReservations.Delete(key)

		return true
	})
	quotaReservations.Range(func(key, _ any) bool {
		quotaReservations.Delete(key)

		return true
	})
}

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
			name: "pool without ipConfig",
			fip: &rfmv2.FloatingIP{
				ObjectMeta: fip.ObjectMeta,
				Spec: rfmv2.FloatingIPSpec{
					FloatingIPPool: "test-pool",
					IPAddr:         &ipAddr,
				},
			},
			existingPools:   []runtime.Object{&rfmv2.FloatingIPPool{TypeMeta: fipPool.TypeMeta, ObjectMeta: fipPool.ObjectMeta}},
			existingPLBCs:   []runtime.Object{plbc},
			expectedAllowed: false,
			expectedMessage: "floatingippool test-pool has no ipConfig",
		},
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
								Used: 1, // ignored: usage comes from the live CR below
							},
						},
					},
				},
			},
			// The pool's only slot is held by a live CR: usage is computed
			// from live state, not quota.status (REPORT.md M7).
			existingFIPs: []runtime.Object{
				&rfmv2.FloatingIP{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "rancher.k8s.binbash.org/v1beta2",
						Kind:       "FloatingIP",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-fip",
						Namespace: "default",
						Labels:    fip.Labels,
					},
					Spec: rfmv2.FloatingIPSpec{
						FloatingIPPool: "test-pool",
					},
					Status: rfmv2.FloatingIPStatus{
						IPAddr: "192.168.1.50",
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
			clearAdmissionState()

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
			dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				{Group: "rancher.k8s.binbash.org", Version: "v1beta2", Resource: "floatingips"}: "FloatingIPList",
			}, allObjects...)

			response := validateFloatingIP(context.Background(), dynamicClient, ar, tc.fip, nil, nil)

			assert.Equal(t, tc.expectedAllowed, response.Allowed)
			if !tc.expectedAllowed {
				assert.Equal(t, tc.expectedMessage, response.Result.Message)
			}
		})
	}
}

// TestValidateFloatingIPDuplicateIPRace pins the REPORT.md M6 fix: the
// duplicate-IP admission check must consult the live FloatingIP list, not
// only pool.status.Allocated, which the manager controller writes
// asynchronously after the CR persists — two concurrent CREATEs of the same
// spec.ipAddr used to both pass while both statuses were still empty. The
// decision is additionally serialized per pool via lockPoolForAdmission.
func TestValidateFloatingIPDuplicateIPRace(t *testing.T) {
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
					Start: "192.168.1.10",
					End:   "192.168.1.200",
				},
			},
		},
		Status: rfmv2.FloatingIPPoolStatus{
			// Deliberately empty: the manager controller has not yet
			// written the concurrent allocation into pool status.
			Allocated: map[string]string{},
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
				"test-pool": {Used: 0},
			},
		},
	}

	newFIPCR := func(name string, pool string, ip string, deleting bool) *rfmv2.FloatingIP {
		fip := &rfmv2.FloatingIP{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rancher.k8s.binbash.org/v1beta2",
				Kind:       "FloatingIP",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					"rancher.k8s.binbash.org/project-name": "test-project",
				},
			},
			Spec: rfmv2.FloatingIPSpec{
				FloatingIPPool: pool,
				IPAddr:         &ip,
			},
		}
		if deleting {
			// A real timestamp: the zero value marshals as null through
			// the unstructured round-trip and would be dropped.
			fip.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			fip.Finalizers = []string{"floatingip-cleanup"}
		}

		return fip
	}

	newFailedFIPCR := func(name string, pool string, ip string, withStatusIP bool) *rfmv2.FloatingIP {
		fip := newFIPCR(name, pool, ip, false)
		fip.Status = rfmv2.FloatingIPStatus{State: "Error"}
		if withStatusIP {
			fip.Status.IPAddr = ip
		}

		return fip
	}

	ipv6Pool := &rfmv2.FloatingIPPool{
		TypeMeta:   fipPool.TypeMeta,
		ObjectMeta: fipPool.ObjectMeta,
		Spec: rfmv2.FloatingIPPoolSpec{
			IPConfig: &rfmv2.IPConfig{
				Subnet: "2001:db8::/64",
				Pool: rfmv2.Pool{
					Start: "2001:db8::10",
					End:   "2001:db8::200",
				},
			},
		},
		Status: rfmv2.FloatingIPPoolStatus{
			Allocated: map[string]string{},
			Available: 1,
		},
	}

	newRequest := func(ip string) *rfmv2.FloatingIP {
		return &rfmv2.FloatingIP{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-fip",
				Namespace: "default",
				Labels: map[string]string{
					"rancher.k8s.binbash.org/project-name": "test-project",
				},
			},
			Spec: rfmv2.FloatingIPSpec{
				FloatingIPPool: "test-pool",
				IPAddr:         &ip,
			},
		}
	}

	testCases := []struct {
		name            string
		fip             *rfmv2.FloatingIP
		oldFIP          *rfmv2.FloatingIP
		pool            *rfmv2.FloatingIPPool
		existingFIPs    []runtime.Object
		expectedAllowed bool
		expectedMessage string
	}{
		{
			name: "live FIP CR claims the requested IP while pool status is empty",
			fip:  newRequest(ipAddr),
			existingFIPs: []runtime.Object{
				newFIPCR("existing-fip", "test-pool", ipAddr, false),
			},
			expectedAllowed: false,
			expectedMessage: "requested IP 192.168.1.100 is already claimed by floatingip default/existing-fip",
		},
		{
			name: "same IP claimed in another pool is not a conflict",
			fip:  newRequest(ipAddr),
			existingFIPs: []runtime.Object{
				newFIPCR("existing-fip", "other-pool", ipAddr, false),
			},
			expectedAllowed: true,
		},
		{
			name: "terminating FIP CR still holds the IP until its finalizer is removed",
			fip:  newRequest(ipAddr),
			existingFIPs: []runtime.Object{
				newFIPCR("existing-fip", "test-pool", ipAddr, true),
			},
			expectedAllowed: false,
			expectedMessage: "requested IP 192.168.1.100 is still held by terminating floatingip default/existing-fip",
		},
		{
			name: "failed-allocation CR without a status IP does not block the retry",
			fip:  newRequest(ipAddr),
			existingFIPs: []runtime.Object{
				newFailedFIPCR("existing-fip", "test-pool", ipAddr, false),
			},
			expectedAllowed: true,
		},
		{
			name: "failed-allocation CR that still holds a status IP blocks the IP",
			fip:  newRequest(ipAddr),
			existingFIPs: []runtime.Object{
				newFailedFIPCR("existing-fip", "test-pool", ipAddr, true),
			},
			expectedAllowed: false,
			expectedMessage: "requested IP 192.168.1.100 is already claimed by floatingip default/existing-fip",
		},
		{
			name: "IPv6 textual forms of the same address conflict",
			fip: func() *rfmv2.FloatingIP {
				fip := newRequest("2001:db8::15")
				fip.Spec.FloatingIPPool = "test-pool"

				return fip
			}(),
			pool: ipv6Pool,
			existingFIPs: []runtime.Object{
				newFIPCR("existing-fip", "test-pool", "2001:db8:0:0:0:0:0:15", false),
			},
			expectedAllowed: false,
			expectedMessage: "requested IP 2001:db8::15 is already claimed by floatingip default/existing-fip",
		},
		{
			name:   "update re-submitting the same IP skips the object itself",
			fip:    newRequest(ipAddr),
			oldFIP: newFIPCR("test-fip", "test-pool", ipAddr, false),
			existingFIPs: []runtime.Object{
				// Status.IPAddr not yet written, so the outer
				// oldFIP.Status.IPAddr skip does not fire; the live list
				// contains the object under update itself.
				newFIPCR("test-fip", "test-pool", ipAddr, false),
			},
			expectedAllowed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reservations taken by an earlier subtest must not leak in.
			clearAdmissionState()

			pool := tc.pool
			if pool == nil {
				pool = fipPool
			}

			ar := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UID: "test-uid",
				},
			}
			unstructuredPools, err := getUnstructuredList([]runtime.Object{pool})
			require.NoError(t, err)
			unstructuredPLBCs, err := getUnstructuredList([]runtime.Object{plbc})
			require.NoError(t, err)
			unstructuredFIPs, err := getUnstructuredList(tc.existingFIPs)
			require.NoError(t, err)

			allObjects := append(unstructuredPools, unstructuredPLBCs...)
			allObjects = append(allObjects, unstructuredFIPs...)
			dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				{Group: "rancher.k8s.binbash.org", Version: "v1beta2", Resource: "floatingips"}: "FloatingIPList",
			}, allObjects...)

			response := validateFloatingIP(context.Background(), dynamicClient, ar, tc.fip, tc.oldFIP, nil)

			assert.Equal(t, tc.expectedAllowed, response.Allowed)
			if !tc.expectedAllowed {
				assert.Equal(t, tc.expectedMessage, response.Result.Message)
			}
		})
	}

	// A near-simultaneous second CREATE of the same IP must be denied by
	// the reservation even before either CR is visible in the live list:
	// the first admission reserved the address, and the per-pool mutex
	// alone cannot cover the window between its response and the CR
	// persisting (the exact REPORT.md M6 trigger).
	t.Run("reservation denies a second near-simultaneous create", func(t *testing.T) {
		clearAdmissionState()

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UID: "test-uid",
			},
		}
		unstructuredPools, err := getUnstructuredList([]runtime.Object{fipPool})
		require.NoError(t, err)
		unstructuredPLBCs, err := getUnstructuredList([]runtime.Object{plbc})
		require.NoError(t, err)

		dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Group: "rancher.k8s.binbash.org", Version: "v1beta2", Resource: "floatingips"}: "FloatingIPList",
		}, append(unstructuredPools, unstructuredPLBCs...)...)

		first := validateFloatingIP(context.Background(), dynamicClient, ar, newRequest(ipAddr), nil, nil)
		require.True(t, first.Allowed, "first admission must be allowed: %+v", first.Result)

		secondFIP := newRequest(ipAddr)
		secondFIP.Name = "test-fip-2"
		second := validateFloatingIP(context.Background(), dynamicClient, ar, secondFIP, nil, nil)
		assert.False(t, second.Allowed)
		assert.Equal(t, "requested IP 192.168.1.100 is pending allocation in floatingippool test-pool", second.Result.Message)
	})

	// The controller's spec rewrite right after a pinned-IP create arrives
	// as an UPDATE of the same object while status.ipAddr is still empty,
	// i.e. while the create's reservation is still live. That write owns
	// the address already and must not be self-denied by the reservation
	// probe.
	t.Run("controller spec update of a just-created pinned IP is not self-denied", func(t *testing.T) {
		clearAdmissionState()

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UID: "test-uid",
			},
		}
		unstructuredPools, err := getUnstructuredList([]runtime.Object{fipPool})
		require.NoError(t, err)
		unstructuredPLBCs, err := getUnstructuredList([]runtime.Object{plbc})
		require.NoError(t, err)

		dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Group: "rancher.k8s.binbash.org", Version: "v1beta2", Resource: "floatingips"}: "FloatingIPList",
		}, append(unstructuredPools, unstructuredPLBCs...)...)

		created := newRequest(ipAddr)
		first := validateFloatingIP(context.Background(), dynamicClient, ar, created, nil, nil)
		require.True(t, first.Allowed, "create admission must be allowed: %+v", first.Result)

		// The UPDATE carries the old object with the same pinned spec IP
		// and empty status, as the controller's spec rewrite does.
		update := validateFloatingIP(context.Background(), dynamicClient, ar, created, created, nil)
		assert.True(t, update.Allowed, "spec update of the object's own address must be allowed: %+v", update.Result)
	})
}

// TestValidateFloatingIPQuotaEnforcement pins the REPORT.md M7 fix: the
// quota decision is serialized per project/pool, computed from live
// cluster state instead of the asynchronously written quota.status, and
// counts each in-flight admission exactly once (reservations paired off
// against the persisted CRs they turned into). The former design slept
// 2s per quota-checked admission — burning admission budget on every
// request while N concurrent creates still all read the same stale Used
// count and all fit under the quota.
func TestValidateFloatingIPQuotaEnforcement(t *testing.T) {
	quotaPool := &rfmv2.FloatingIPPool{
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
					Start: "192.168.1.10",
					End:   "192.168.1.200",
				},
			},
		},
		Status: rfmv2.FloatingIPPoolStatus{
			Available: 5,
		},
	}

	newPlbc := func(quota int, statusUsed int) *rfmv2.FloatingIPProjectQuota {
		return &rfmv2.FloatingIPProjectQuota{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rancher.k8s.binbash.org/v1beta2",
				Kind:       "FloatingIPProjectQuota",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-project",
			},
			Spec: rfmv2.FloatingIPProjectQuotaSpec{
				FloatingIPQuota: map[string]int{
					"test-pool": quota,
				},
			},
			Status: rfmv2.FloatingIPProjectQuotaStatus{
				// Deliberately inconsistent with the seeded CRs: status
				// must not be consulted for the decision.
				FloatingIPs: map[string]*rfmv2.FipInfo{
					"test-pool": {Used: statusUsed},
				},
			},
		}
	}

	newRequest := func(name string) *rfmv2.FloatingIP {
		return &rfmv2.FloatingIP{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rancher.k8s.binbash.org/v1beta2",
				Kind:       "FloatingIP",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					"rancher.k8s.binbash.org/project-name": "test-project",
				},
			},
			Spec: rfmv2.FloatingIPSpec{
				FloatingIPPool: "test-pool",
			},
		}
	}

	// newCR seeds a persisted CR as the manager controller would leave it
	// after granting (ipAddr set) or while still allocating (pending).
	newCR := func(name string, ip string, state string, recent bool) *rfmv2.FloatingIP {
		fip := &rfmv2.FloatingIP{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rancher.k8s.binbash.org/v1beta2",
				Kind:       "FloatingIP",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					"rancher.k8s.binbash.org/project-name": "test-project",
				},
			},
			Spec: rfmv2.FloatingIPSpec{
				FloatingIPPool: "test-pool",
			},
		}
		if ip != "" {
			fip.Status.IPAddr = ip
		}
		if state != "" {
			fip.Status.State = state
		}
		if recent {
			fip.CreationTimestamp = metav1.Now()
		}

		return fip
	}

	newClient := func(plbc *rfmv2.FloatingIPProjectQuota, fips ...*rfmv2.FloatingIP) dynamic.Interface {
		objects := []runtime.Object{}
		unstructuredPools, err := getUnstructuredList([]runtime.Object{quotaPool})
		require.NoError(t, err)
		unstructuredPLBCs, err := getUnstructuredList([]runtime.Object{plbc})
		require.NoError(t, err)
		objects = append(objects, unstructuredPools...)
		objects = append(objects, unstructuredPLBCs...)
		for _, fip := range fips {
			unstructuredFIPs, err := getUnstructuredList([]runtime.Object{fip})
			require.NoError(t, err)
			objects = append(objects, unstructuredFIPs...)
		}

		return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Group: "rancher.k8s.binbash.org", Version: "v1beta2", Resource: "floatingips"}: "FloatingIPList",
		}, objects...)
	}

	validate := func(t *testing.T, client dynamic.Interface, fip *rfmv2.FloatingIP, oldFIP *rfmv2.FloatingIP) *admissionv1.AdmissionResponse {
		t.Helper()

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				UID: "test-uid",
			},
		}

		return validateFloatingIP(context.Background(), client, ar, fip, oldFIP, nil)
	}

	denyMessage := func(quota, used int) string {
		return fmt.Sprintf("quota exceeded for floatingippool test-pool in project test-project. Quota: %d, Used: %d", quota, used)
	}

	t.Run("burst of creates is capped at the quota without any sleep", func(t *testing.T) {
		clearAdmissionState()

		client := newClient(newPlbc(2, 0))

		first := validate(t, client, newRequest("fip-1"), nil)
		require.True(t, first.Allowed, "first create must be allowed: %+v", first.Result)

		second := validate(t, client, newRequest("fip-2"), nil)
		require.True(t, second.Allowed, "second create must be allowed: %+v", second.Result)

		third := validate(t, client, newRequest("fip-3"), nil)
		assert.False(t, third.Allowed)
		assert.Equal(t, denyMessage(2, 2), third.Result.Message)
	})

	t.Run("allocated CRs pair off their reservations so sequential creates are not double-counted", func(t *testing.T) {
		clearAdmissionState()

		// Two admissions allowed against an empty cluster; the manager
		// controller grants both within the reservation TTL.
		emptyClient := newClient(newPlbc(2, 0))
		require.True(t, validate(t, emptyClient, newRequest("fip-1"), nil).Allowed)
		require.True(t, validate(t, emptyClient, newRequest("fip-2"), nil).Allowed)

		// Both CRs now exist and hold IPs. Each must be counted exactly
		// once: allocated CRs pair off their still-live reservations.
		allocatedClient := newClient(newPlbc(3, 0), newCR("fip-1", "192.168.1.10", "", true), newCR("fip-2", "192.168.1.11", "", true))

		third := validate(t, allocatedClient, newRequest("fip-3"), nil)
		assert.True(t, third.Allowed, "third create must be allowed with 2 of 3 slots used: %+v", third.Result)

		fourth := validate(t, allocatedClient, newRequest("fip-4"), nil)
		assert.False(t, fourth.Allowed)
		assert.Equal(t, denyMessage(3, 3), fourth.Result.Message)
	})

	t.Run("persisted pending CRs count without double-counting their reservations", func(t *testing.T) {
		clearAdmissionState()

		emptyClient := newClient(newPlbc(2, 0))
		require.True(t, validate(t, emptyClient, newRequest("fip-1"), nil).Allowed)

		// fip-1 persisted but the manager controller has not granted an
		// IP yet: it occupies a slot, and it pairs off its reservation.
		pendingClient := newClient(newPlbc(2, 0), newCR("fip-1", "", "", true))

		second := validate(t, pendingClient, newRequest("fip-2"), nil)
		assert.True(t, second.Allowed, "second create must be allowed with 1 of 2 slots used: %+v", second.Result)

		third := validate(t, pendingClient, newRequest("fip-3"), nil)
		assert.False(t, third.Allowed)
		assert.Equal(t, denyMessage(2, 2), third.Result.Message)
	})

	t.Run("failed-allocation CR and stale quota status do not consume a slot", func(t *testing.T) {
		clearAdmissionState()

		client := newClient(newPlbc(1, 1), newCR("failed-fip", "", "Error", true))

		response := validate(t, client, newRequest("fip-1"), nil)
		assert.True(t, response.Allowed, "failed allocation must not block the retry: %+v", response.Result)
	})

	t.Run("terminating CR still consumes a slot", func(t *testing.T) {
		clearAdmissionState()

		terminating := newCR("fip-1", "192.168.1.10", "", true)
		terminating.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
		terminating.Finalizers = []string{"floatingip-cleanup"}

		response := validate(t, newClient(newPlbc(1, 0), terminating), newRequest("fip-2"), nil)
		assert.False(t, response.Allowed)
		assert.Equal(t, denyMessage(1, 1), response.Result.Message)
	})

	t.Run("other project's CRs do not consume a slot", func(t *testing.T) {
		clearAdmissionState()

		other := newCR("fip-1", "192.168.1.10", "", true)
		other.Labels = map[string]string{"rancher.k8s.binbash.org/project-name": "other-project"}

		response := validate(t, newClient(newPlbc(1, 0), other), newRequest("fip-2"), nil)
		assert.True(t, response.Allowed, "another project's allocation must not count here: %+v", response.Result)
	})

	t.Run("same-pool update is not quota-checked", func(t *testing.T) {
		clearAdmissionState()

		self := newRequest("test-fip")
		self.Status.IPAddr = "192.168.1.10"
		oldFIP := self.DeepCopy()
		client := newClient(newPlbc(1, 0), self)

		// Quota is 1 and the pool is full, but an update of the object
		// that owns the slot cannot change occupancy.
		response := validate(t, client, self, oldFIP)
		assert.True(t, response.Allowed, "same-pool update must not be self-denied: %+v", response.Result)

		// No reservation may have been taken: the slot was already live.
		other := newRequest("fip-2")
		denial := validate(t, client, other, nil)
		assert.False(t, denial.Allowed)
		assert.Equal(t, denyMessage(1, 1), denial.Result.Message)
	})

	t.Run("aged pending CRs count directly but do not mask live reservations", func(t *testing.T) {
		clearAdmissionState()

		// Zero CreationTimestamp: older than the reservation TTL, so this
		// CR never had a live reservation to consume (manager backlog).
		aged := newCR("aged-fip", "", "", false)

		// Coverage continuity: the aged pending CR occupies a slot on its
		// own.
		client := newClient(newPlbc(2, 0), aged)
		first := validate(t, client, newRequest("fip-1"), nil)
		require.True(t, first.Allowed, "aged pending CR occupies one of two slots: %+v", first.Result)

		// The reservation the first admission just took must not be
		// paired off against the aged CR.
		second := validate(t, client, newRequest("fip-2"), nil)
		assert.False(t, second.Allowed)
		assert.Equal(t, denyMessage(2, 2), second.Result.Message)
	})

	t.Run("update of an errored CR that holds no slot is quota-checked", func(t *testing.T) {
		clearAdmissionState()

		errored := newCR("test-fip", "", "Error", true)
		allocatedOther := newCR("other-fip", "192.168.1.10", "", true)
		client := newClient(newPlbc(1, 0), errored, allocatedOther)

		// The errored CR holds no slot, so re-reconciling it can occupy
		// one; with the pool's only slot taken it must be denied.
		response := validate(t, client, errored, errored.DeepCopy())
		assert.False(t, response.Allowed)
		assert.Equal(t, denyMessage(1, 1), response.Result.Message)
	})

	// M7(b) is about concurrency: N simultaneous creates for one
	// project/pool must serialize on the per-key lock so exactly quota
	// many pass. Without lockQuotaForAdmission every goroutine reads the
	// same empty list and all 8 would be allowed.
	t.Run("concurrent creates are serialized per project/pool", func(t *testing.T) {
		clearAdmissionState()

		client := newClient(newPlbc(3, 0))

		const goroutines = 8
		results := make([]*admissionv1.AdmissionResponse, goroutines)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ar := &admissionv1.AdmissionReview{
					Request: &admissionv1.AdmissionRequest{
						UID: types.UID(fmt.Sprintf("uid-%d", i)),
					},
				}
				results[i] = validateFloatingIP(context.Background(), client, ar, newRequest(fmt.Sprintf("fip-%d", i)), nil, nil)
			}(i)
		}
		close(start)
		wg.Wait()

		allowed := 0
		for _, r := range results {
			require.NotNil(t, r)
			if r.Allowed {
				allowed++
			}
		}
		assert.Equal(t, 3, allowed, "exactly quota-many concurrent creates may pass")
	})
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
			name: "missing ipConfig",
			fipPool: &rfmv2.FloatingIPPool{
				TypeMeta:   validFipPool.TypeMeta,
				ObjectMeta: validFipPool.ObjectMeta,
				Spec:       rfmv2.FloatingIPPoolSpec{},
			},
			expectedAllowed: false,
			expectedMessage: "ipConfig is required in floatingippool spec",
		},
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

// REPORT.md M8: a body that decodes into an AdmissionReview without a
// `request` field (e.g. `{}`) must yield a structured denial, not a
// nil-pointer panic on ar.Request.Object.Raw followed by a connection reset.
func TestAdmissionReviewWithoutRequestIsDenied(t *testing.T) {
	cases := []struct {
		body     string
		wantCode int
	}{
		{`{}`, http.StatusBadRequest},
		{`{"kind":"AdmissionReview","apiVersion":"admission.k8s.io/v1"}`, http.StatusBadRequest},
		{`{"request":null}`, http.StatusBadRequest},
		// request present but object absent: pins the 500-vs-panic boundary —
		// the empty ar.Request.Object.Raw must hit the unmarshal-error path.
		{`{"request":{"uid":"x"}}`, http.StatusInternalServerError},
	}
	handlers := map[string]func(w http.ResponseWriter, r *http.Request){
		"floatingip":     (&Handler{ctx: context.Background()}).validateFloatingIPAdmission,
		"floatingippool": (&Handler{ctx: context.Background()}).validateFloatingIPPoolAdmission,
	}

	for endpoint, handler := range handlers {
		for _, tc := range cases {
			t.Run(endpoint+"/"+tc.body, func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler(rec, httptest.NewRequest(http.MethodPost, "/validate-"+endpoint, strings.NewReader(tc.body)))

				assert.Equal(t, tc.wantCode, rec.Code, "body %q", tc.body)
				if tc.wantCode != http.StatusBadRequest {
					// pre-existing plain-text error path, no AdmissionReview body
					return
				}
				assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

				var review admissionv1.AdmissionReview
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &review))
				require.NotNil(t, review.Response)
				assert.False(t, review.Response.Allowed)
				assert.Equal(t, int32(tc.wantCode), review.Response.Result.Code)
			})
		}
	}
}
