# rancher-fip-lb-controller

> Note: this component has been merged into the [rancher-fip-manager](https://github.com/joeyloman/rancher-fip-manager) repository (this directory, `lbcontroller/`, is a separate Go module). Build the image from the repository root with `make docker-build-lbcontroller` / `make docker-push-lbcontroller`; CI publishes it as `ghcr.io/joeyloman/rancher-fip-lb-controller`. The Helm chart lives at `deployments/charts/rancher-fip-lb-controller` and is published to `oci://ghcr.io/joeyloman/charts`.

## Overview

The `rancher-fip-lb-controller` is a Kubernetes controller that manages MetalLB `IPAddressPool` and `L2Advertisement` and PureLB `ServiceGroup` and `LBNodeAgent` resources based on `LoadBalancer` services in a Rancher-managed cluster. It integrates with an external IPAM solution to request and release floating IP addresses.

## Building the Controller

Build the binary and tests from this directory:

```sh
cd lbcontroller
go build -o ../bin/rancher-fip-lb-controller ./cmd/lb-controller
go test ./...
```

Build the Docker image from the repository root:

```sh
make docker-build-lbcontroller LB_CONTROLLER_IMG=your-registry/rancher-fip-lb-controller:latest
```

## Deploying the Controller

### Using `kubectl`

Create the application namespace (if it doesn't exist yet), then apply the manifests from the `deployments` directory:

```sh
kubectl create namespace rancher-fip-manager
kubectl apply -f lbcontroller/deployments/deployment.yaml
```

This will create the following resources in the `rancher-fip-manager` namespace:
-   A `rancher-fip-lb-controller` `ServiceAccount`.
-   A `ClusterRole` and `ClusterRoleBinding` with the necessary permissions.
-   A `Role` and `RoleBinding` for Secrets access within the namespace.
-   A `Deployment` for the controller.

### Using Helm

```sh
helm upgrade --install rancher-fip-lb-controller \
  deployments/charts/rancher-fip-lb-controller \
  -n rancher-fip-manager --create-namespace
```

## Configuration

The controller is configured via a `Secret` and a `ConfigMap` in the `rancher-fip-manager` namespace.

### Project detection

The controller determines the Rancher project ID as follows:
-   First, it checks the `field.cattle.io/projectId` label on the Service's namespace.
-   If not present, it falls back to the `rancher.k8s.binbash.org/project-name` label on the controller's app namespace (`rancher-fip-manager`).

For HTTP server enablement and the fallback to work, label the app namespace:

```sh
kubectl label namespace rancher-fip-manager rancher.k8s.binbash.org/project-name=<project-id>
```

### IPAM Configuration

For each Rancher project that will use the controller, you must create a `Secret` with the following format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: rancher-fip-config-<project-id> # e.g. rancher-fip-config-p-12345
  namespace: rancher-fip-manager
stringData:
  apiUrl: "http://your-ipam-api"
  clientSecret: "your-client-secret"
  cluster: "your-cluster-name"
  project: "your-project-name"
  floatingIPPool: "your-fip-pool"
  loadBalancerType: "<metallb or purelb>"
```

## Service Annotations and Labels

To request a specific static IP address for a `LoadBalancer` service, add the following annotation to your service manifest:

```yaml
metadata:
  annotations:
    rancher.k8s.binbash.org/static-ip: "<ip-address>"
```

To request a specific floatingIPPool for a `LoadBalancer` service, add the following annotation to your service manifest:

```yaml
metadata:
  annotations:
    rancher.k8s.binbash.org/static-network: "<floatingIPPool-name>"
```

To request a specific floatingIPGroup for a `LoadBalancer` service for multiple port usage on a shared ip, add the following annotation to your service manifest:

```yaml
metadata:
  annotations:
    rancher.k8s.binbash.org/floatingip-group: "<floatingIPGroup-name>"
```

The controller will add the following annotations and labels to the service after requesting the floating IP:

- Annotation `rancher.k8s.binbash.org/floatingip`: The allocated floating IP address.
- Annotation `rancher.k8s.binbash.org/floatingip-project`: The project the address was allocated under. Deletion uses it to release the address, so it stays correct if the namespace's project label changes later.
- Annotation `metallb.io/allow-shared-ip`: The shared key for MetalLB to allow sharing the IP with other services.
- Annotation `purelb.io/service-group`: The PureLB service group name.
- Annotation `purelb.io/allow-shared-ip`: The shared key for PureLB to allow sharing the IP with other services.
- Label `rancher.k8s.binbash.org/service`: The name of the service.
- Label `rancher.k8s.binbash.org/servicenamespace`: The namespace of the service.

The controller also adds a finalizer `rancher.k8s.binbash.org/floatingip-cleanup` to the service to ensure proper cleanup of resources when the service is deleted.

## HTTP Management UI

When the app namespace carries the project label, every replica serves the
management UI on port 8080, but only the elected leader is added to the
`Service`: the leader labels its own Pod with
`rancher-fip-lb-controller/leader=true`, and the Service selects on that label.
Requests that mutate state (`/release`, `/remove`) are refused with `503` on a
non-leader replica, and the label is cleared when leadership is lost.

Sessions are kept in memory per replica, so a leader change requires logging in
again.

Liveness (`/healthz`) and readiness (`/readyz`) probes are served on port 8081
on every replica, including deployments where the UI is not configured.

## Network Interface Mappings

You must create a `ConfigMap` in the `rancher-fip-manager` namespace that maps floating IP pools to network interfaces for MetalLB's `L2Advertisement` or PureLB's `LBNodeAgent`.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: network-interface-mappings
  namespace: rancher-fip-manager
data:
  # <floating_ip_pool_name>: <network_interface>
  pool1: eth0
  pool2: eth1
```

## License

Copyright (c) 2026 Joey Loman <joey@binbash.org>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.