package floatingip

import (
	"testing"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseServicesFromLabels(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected []v1beta2.AssignedService
	}{
		{
			name:     "nil labels",
			labels:   nil,
			expected: nil,
		},
		{
			name:     "empty labels",
			labels:   map[string]string{},
			expected: nil,
		},
		{
			name: "single service with namespace and name",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-namespace": "default",
				"rancher.k8s.binbash.org/service-0-name":      "my-service",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "my-service"},
			},
		},
		{
			name: "multiple services",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-namespace": "default",
				"rancher.k8s.binbash.org/service-0-name":      "service-0",
				"rancher.k8s.binbash.org/service-1-namespace": "kube-system",
				"rancher.k8s.binbash.org/service-1-name":      "service-1",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "service-0"},
				{ServiceNamespace: "kube-system", ServiceName: "service-1"},
			},
		},
		{
			name: "service with only namespace",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "default"},
			},
		},
		{
			name: "service with only name",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-name": "my-service",
			},
			expected: []v1beta2.AssignedService{
				{ServiceName: "my-service"},
			},
		},
		{
			name: "services out of order",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-2-namespace": "ns-2",
				"rancher.k8s.binbash.org/service-2-name":      "svc-2",
				"rancher.k8s.binbash.org/service-0-namespace": "ns-0",
				"rancher.k8s.binbash.org/service-0-name":      "svc-0",
				"rancher.k8s.binbash.org/service-1-namespace": "ns-1",
				"rancher.k8s.binbash.org/service-1-name":      "svc-1",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "ns-0", ServiceName: "svc-0"},
				{ServiceNamespace: "ns-1", ServiceName: "svc-1"},
				{ServiceNamespace: "ns-2", ServiceName: "svc-2"},
			},
		},
		{
			name: "non-matching labels ignored",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-namespace": "default",
				"rancher.k8s.binbash.org/service-0-name":      "my-service",
				"app.kubernetes.io/name":                      "my-app",
				"version":                                     "v1",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "my-service"},
			},
		},
		{
			name: "invalid label format ignored",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-invalid":   "value",
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "default"},
			},
		},
		{
			name: "service with empty fields excluded",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-namespace": "",
				"rancher.k8s.binbash.org/service-0-name":      "",
			},
			expected: nil,
		},
		{
			name: "mixed valid and empty services",
			labels: map[string]string{
				"rancher.k8s.binbash.org/service-0-namespace": "default",
				"rancher.k8s.binbash.org/service-0-name":      "my-service",
				"rancher.k8s.binbash.org/service-1-namespace": "",
				"rancher.k8s.binbash.org/service-1-name":      "",
			},
			expected: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "my-service"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseServicesFromLabels(tt.labels)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDescribeServices(t *testing.T) {
	tests := []struct {
		name     string
		services []v1beta2.AssignedService
		expected string
	}{
		{
			name:     "empty services",
			services: []v1beta2.AssignedService{},
			expected: "no services",
		},
		{
			name:     "nil services",
			services: nil,
			expected: "no services",
		},
		{
			name: "single service with namespace and name",
			services: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "my-service"},
			},
			expected: "default/my-service",
		},
		{
			name: "single service with only namespace",
			services: []v1beta2.AssignedService{
				{ServiceNamespace: "default"},
			},
			expected: "default",
		},
		{
			name: "single service with only name",
			services: []v1beta2.AssignedService{
				{ServiceName: "my-service"},
			},
			expected: "my-service",
		},
		{
			name: "multiple services",
			services: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "service-0"},
				{ServiceNamespace: "kube-system", ServiceName: "service-1"},
			},
			expected: "services default/service-0, kube-system/service-1",
		},
		{
			name: "services with mixed fields",
			services: []v1beta2.AssignedService{
				{ServiceNamespace: "default", ServiceName: "service-0"},
				{ServiceName: "service-1-only-name"},
				{ServiceNamespace: "service-2-only-ns"},
			},
			expected: "services default/service-0, service-1-only-name, service-2-only-ns",
		},
		{
			name: "services with empty fields",
			services: []v1beta2.AssignedService{
				{ServiceNamespace: "", ServiceName: ""},
				{ServiceNamespace: "default", ServiceName: "my-service"},
			},
			expected: "default/my-service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := describeServices(tt.services)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseServicesFromLabels_Integration(t *testing.T) {
	// Integration test with realistic Rancher service labels
	t.Run("realistic Rancher service labels", func(t *testing.T) {
		labels := map[string]string{
			"rancher.k8s.binbash.org/service-0-namespace": "cattle-system",
			"rancher.k8s.binbash.org/service-0-name":      "rancher",
			"rancher.k8s.binbash.org/service-1-namespace": "kube-system",
			"rancher.k8s.binbash.org/service-1-name":      "kube-controller-manager",
			"rancher.k8s.binbash.org/service-2-namespace": "kube-system",
			"rancher.k8s.binbash.org/service-2-name":      "kube-proxy",
			"app.kubernetes.io/managed-by":                "Helm",
			"helm.sh/chart":                               "rancher-0.0.1",
		}

		result := parseServicesFromLabels(labels)
		require.Len(t, result, 3)

		assert.Equal(t, "cattle-system", result[0].ServiceNamespace)
		assert.Equal(t, "rancher", result[0].ServiceName)
		assert.Equal(t, "kube-system", result[1].ServiceNamespace)
		assert.Equal(t, "kube-controller-manager", result[1].ServiceName)
		assert.Equal(t, "kube-system", result[2].ServiceNamespace)
		assert.Equal(t, "kube-proxy", result[2].ServiceName)
	})
}

func TestDescribeServices_Integration(t *testing.T) {
	// Integration test with realistic service descriptions
	t.Run("realistic service description", func(t *testing.T) {
		services := []v1beta2.AssignedService{
			{ServiceNamespace: "cattle-system", ServiceName: "rancher"},
			{ServiceNamespace: "kube-system", ServiceName: "kube-controller-manager"},
			{ServiceNamespace: "kube-system", ServiceName: "kube-proxy"},
		}

		result := describeServices(services)
		assert.Equal(t, "services cattle-system/rancher, kube-system/kube-controller-manager, kube-system/kube-proxy", result)
	})
}
