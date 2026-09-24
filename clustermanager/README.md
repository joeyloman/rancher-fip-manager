# rancher-fip-cluster-manager

## Overview

`rancher-fip-cluster-manager` is a Kubernetes controller designed to run within a Rancher management cluster. Its primary purpose is to automate the setup and configuration of networking components in downstream Kubernetes clusters managed by Rancher. It achieves this by watching for newly provisioned clusters and specific custom resources, then deploying necessary Helm charts and managing secrets required for integration with a Floating IP management system.

The controller follows the standard Kubernetes operator pattern, ensuring robust and automated lifecycle management of resources across multiple clusters.

> Note: this component has been merged into the [rancher-fip-manager](https://github.com/joeyloman/rancher-fip-manager) repository (this directory, `clustermanager/`, is a separate Go module resolved against the root module via a local `replace` directive). Build the image from the repository root with `make docker-build-clustermanager` / `make docker-push-clustermanager`; CI publishes it as `ghcr.io/joeyloman/rancher-fip-cluster-manager`. The `rancher-fip-manager` Helm chart in `deployments/charts/rancher-fip-manager` deploys it (templates in `templates/clustermanager/`).

## Features

-   **CRD Watching**: Monitors `management.cattle.io/v3` clusters and `floatingipprojectquota.rancher.k8s.binbash.org` Custom Resources for create events.
-   **Automated Chart Deployment**: Deploys and configures `rancher-fip-lb-controller` and `MetalLB` Helm charts onto downstream clusters.
-   **Secret Management**: Securely generates and distributes API tokens and configuration details as Kubernetes Secrets to both the local Rancher cluster and downstream clusters.
-   **Automated Network Configuration**: Creates a `network-interface-mappings` ConfigMap in downstream clusters based on `FloatingIPPool` resources, automating network setup.
-   **High Availability**: Implements a leader election mechanism to ensure that only one instance of the controller is active at any time, preventing conflicting operations.
-   **Flexible Configuration**:
    -   Utilizes a central `rancher-fip-config` ConfigMap in the app namespace (defaults to `rancher-fip-manager`) to define Helm chart versions, repositories, and other operational parameters.
    -   Supports running both in-cluster and locally for development by using `KUBECONFIG` (or falling back to in-cluster service account credentials). A `--kubecontext` flag exists but is not currently used to select a context.
-   **Context-Aware**: The application is built to be context-aware, ensuring graceful shutdowns and robust handling of asynchronous operations.
-   **Logging**: Uses `logrus` for structured, configurable logging.

## Architecture

The application is built using the [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) library, which provides a high-level framework for writing Kubernetes operators.

### Controllers

The manager runs two primary controllers that work concurrently:

1.  **ClusterProvisioning Controller**: This controller watches `management.cattle.io/v3` cluster resources. When a new, eligible downstream cluster labeled with `rancher-fip: enabled` is provisioned, it triggers a reconciliation loop. This loop is responsible for fetching the new cluster's kubeconfig, establishing a connection, and deploying the `rancher-fip-lb-controller` and `MetalLB` Helm charts. It includes special logic to identify the associated project when running on Harvester infrastructure.

2.  **FloatingIPProjectQuota Controller**: This controller watches `floatingipprojectquota.rancher.k8s.binbash.org` resources. When a new `FloatingIPProjectQuota` is created, it signifies that a Rancher project is enabled for Floating IP management. The controller finds the associated project and downstream cluster, then calls a shared handler to generate a unique API token and create corresponding secrets in both the local Rancher cluster and the downstream cluster.

### Shared Handlers & Logic

Beyond the main reconciliation loops, the manager utilizes several shared handlers to centralize common logic:

-   **Secret Handler**: Manages the lifecycle of API token secrets in both local and downstream clusters.
-   **ConfigMap Handler**: Creates and updates the `network-interface-mappings` ConfigMap in downstream clusters, which is used by other components to map network interfaces.
-   **Namespace Handler**: Ensures required namespaces exist and applies identifying labels to link them to Rancher projects.

### Leader Election

To support running in a high-availability configuration with multiple replicas, the controller manager uses the standard Kubernetes leader election pattern (`leaderelection` from `k8s/client-go`). The first pod to acquire a `LeaseLock` becomes the leader and starts the controllers. Other pods remain on standby. If the leader pod fails, another pod will acquire the lease and take over, ensuring continuous operation.

## Configuration
### Command-line flags

The binary supports the following flags:

-   `--app-namespace` (string, default: `rancher-fip-manager`): Namespace where the app runs and where `rancher-fip-config` and optional `cacerts` are read.
-   `--leader-elect` (bool, default: `true`): Enable leader election.
-   `--lease-lock-name` (string, default: `rancher-fip-cluster-manager-lock`): Name of the leader election lock.
-   `--lease-lock-namespace` (string, default: `rancher-fip-manager`): Namespace of the leader election lock.
-   `--kubecontext` (string): Present but currently not used to select a context.


### Environment Variables

The controller connects to Kubernetes using controller-runtime defaults:

-   `KUBECONFIG`: Path to the kubeconfig file. If not set, in-cluster configuration is used when running in Kubernetes.
-   Note: a `--kubecontext` flag exists but is not currently used to switch contexts; the active context in `KUBECONFIG` is used.

### ConfigMap

The controller requires a `ConfigMap` named `rancher-fip-config` in the application namespace (defaults to `rancher-fip-manager`) for its configuration. This `ConfigMap` defines the versions and configuration for the Helm charts it deploys.

*Example `rancher-fip-config` ConfigMap:*
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: rancher-fip-config
  namespace: rancher-fip-manager
data:
  RancherFipApiServerURL: "http://rancher-fip-api.example.com"
  RancherFipLBControllerChartName: "rancher-fip-lb-controller"
  RancherFipLBControllerChartRef: "oci://registry.example.com/projectname/rancher-fip-lb-controller"
  RancherFipLBControllerChartVersion: "0.2.0"
  RancherFipLBControllerNamespace: "rancher-fip-manager"
  RancherFipLBControllerValues: |
    image:
      repository: registry.example.com/rancher/rancher-fip-lb-controller
      tag: v0.2.0
  PureLBControllerChartName: "purelb"
  PureLBControllerChartRef: "https://gitlab.com/api/v4/projects/20400619/packages/helm/stable"
  PureLBControllerChartVersion: "0.13.0"
  PureLBControllerNamespace: "rancher-fip-manager"
  PureLBControllerValues: |
    image:
      repository: registry.gitlab.com/purelb/purelb
      tag: v0.13.0
    lbnodeagent:
      sendgarp: true
      nodeSelector:
        node-role.kubernetes.io/control-plane: 'true'
      tolerations: [ { "effect": "NoSchedule", "key":
      "node-role.kubernetes.io/master", "operator": "Exists" }, { "effect":
      "NoSchedule", "key": "node-role.kubernetes.io/control-plane", "operator":
      "Exists" }, { "effect": "NoExecute", "key": "node-role.kubernetes.io/etcd",
      "operator": "Exists" } ]
    allocator:
      tolerations: [ { "effect": "NoSchedule", "key":
      "node-role.kubernetes.io/master", "operator": "Exists" }, { "effect":
      "NoSchedule", "key": "node-role.kubernetes.io/control-plane", "operator":
      "Exists" }, { "effect": "NoExecute", "key": "node-role.kubernetes.io/etcd",
      "operator": "Exists" } ]  
  MetalLBControllerChartName: "metallb"
  MetalLBControllerChartRef: "https://metallb.github.io/metallb"
  MetalLBControllerChartVersion: "0.15.3"
  MetalLBControllerNamespace: "rancher-fip-manager"
  MetalLBControllerValues: |
    controller:
      image:
        repository: quay.io/metallb/controller
        tag: v0.15.3
      nodeSelector:
        node-role.kubernetes.io/master: 'true'
      tolerations: [ { "effect": "NoSchedule", "key":
      "node-role.kubernetes.io/master", "operator": "Exists" }, { "effect":
      "NoSchedule", "key": "node-role.kubernetes.io/control-plane", "operator":
      "Exists" }, { "effect": "NoExecute", "key": "node-role.kubernetes.io/etcd",
      "operator": "Exists" } ]
    speaker:
      image:
        repository: quay.io/metallb/speaker
        tag: v0.15.2
      nodeSelector:
        node-role.kubernetes.io/master: 'true'
      tolerations: [ { "effect": "NoSchedule", "key":
      "node-role.kubernetes.io/master", "operator": "Exists" }, { "effect":
      "NoSchedule", "key": "node-role.kubernetes.io/control-plane", "operator":
      "Exists" }, { "effect": "NoExecute", "key": "node-role.kubernetes.io/etcd",
      "operator": "Exists" } ]
      frr:
        image:
          repository: quay.io/frrouting/frr
          tag: 10.4.1
```

### Optional CA certificates propagation

If a `Secret` named `cacerts` exists in the application namespace with key `ca.crt`, the controller will propagate this secret to each downstream cluster in the target namespace (matching `RancherFipLBControllerNamespace`). This allows components to trust a private CA used by the FIP API.

### Network interface mappings

The controller creates or updates a `ConfigMap` named `network-interface-mappings` in each downstream cluster (in the application/target namespace). Its data maps:

-   key: `FloatingIPPool.metadata.name`
-   value: `FloatingIPPool.spec.targetNetworkInterface`

Only `FloatingIPPool` objects whose `spec.targetCluster` matches the downstream cluster are included.

### Watch behavior and cluster selection

-   `ClusterProvisioning` reacts to create events for `management.cattle.io/v3.Cluster` and ignores:
    -   the local cluster (`name=local`)
    -   clusters labeled `provider.cattle.io=harvester`
    -   clusters without label `rancher-fip=enabled`
-   `FloatingIPProjectQuota` reacts to create events for `rancher.k8s.binbash.org/v1beta2.FloatingIPProjectQuota` and requires the target cluster to be labeled `rancher-fip=enabled`.

### Load Balancer Type Selection

The controller supports two load balancer implementations: PureLB (default) and MetalLB. The load balancer type is determined by the `rancher-fip-lbtype` label on the cluster resource:

-   **Default behavior**: If the `rancher-fip-lbtype` label is not set or has any value other than `metallb`, the controller will deploy **PureLB** as the load balancer controller.
-   **MetalLB selection**: If the cluster is labeled with `rancher-fip-lbtype=metallb`, the controller will deploy **MetalLB** instead.

The `loadBalancerType` value (either `purelb` or `metallb`) is stored in the secrets created for downstream clusters, allowing other components to be aware of which load balancer implementation is in use. The controller includes validation logic to prevent conflicting installations: if a cluster is configured for one load balancer type but the other is already installed, reconciliation will be skipped to avoid conflicts.


# License

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