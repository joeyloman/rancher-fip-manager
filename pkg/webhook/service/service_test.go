package service

import (
	"context"
	"testing"

	rfmv2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/stretchr/testify/assert"
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
