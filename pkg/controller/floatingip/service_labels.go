package floatingip

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
)

// serviceIndexLabelPrefix is the prefix for indexed service labels
const serviceIndexLabelPrefix = "rancher.k8s.binbash.org/service-"

// parseServicesFromLabels parses indexed service labels into a list of AssignedService
// Labels format: rancher.k8s.binbash.org/service-0-namespace, rancher.k8s.binbash.org/service-0-name
func parseServicesFromLabels(labels map[string]string) []v1beta2.AssignedService {
	if labels == nil {
		return nil
	}

	serviceMap := make(map[int]v1beta2.AssignedService)

	// Regex to match service-index-field labels
	// Matches: service-0-namespace, service-1-name, etc.
	re := regexp.MustCompile(`service-(\d+)-(namespace|name)`)

	for key, value := range labels {
		if !strings.HasPrefix(key, serviceIndexLabelPrefix) {
			continue
		}

		matches := re.FindStringSubmatch(key)
		if len(matches) != 3 {
			continue
		}

		var idx int
		fmt.Sscanf(matches[1], "%d", &idx)
		field := matches[2]

		if _, exists := serviceMap[idx]; !exists {
			serviceMap[idx] = v1beta2.AssignedService{}
		}

		service := serviceMap[idx]
		if field == "namespace" {
			service.ServiceNamespace = value
		} else if field == "name" {
			service.ServiceName = value
		}
		serviceMap[idx] = service
	}

	// Convert map to sorted slice
	var indices []int
	for i := range serviceMap {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	var services []v1beta2.AssignedService
	for _, i := range indices {
		svc := serviceMap[i]
		// Only add if at least one field is set
		if svc.ServiceNamespace != "" || svc.ServiceName != "" {
			services = append(services, svc)
		}
	}

	return services
}

// describeServices creates a human-readable description of services
func describeServices(services []v1beta2.AssignedService) string {
	if len(services) == 0 {
		return "no services"
	}

	var parts []string
	for _, svc := range services {
		if svc.ServiceNamespace != "" && svc.ServiceName != "" {
			parts = append(parts, fmt.Sprintf("%s/%s", svc.ServiceNamespace, svc.ServiceName))
		} else if svc.ServiceName != "" {
			parts = append(parts, svc.ServiceName)
		} else if svc.ServiceNamespace != "" {
			parts = append(parts, svc.ServiceNamespace)
		}
	}

	if len(parts) == 0 {
		return "no services"
	}

	if len(parts) == 1 {
		return parts[0]
	}

	return fmt.Sprintf("services %s", strings.Join(parts, ", "))
}
