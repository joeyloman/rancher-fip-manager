# Image URL to use all building/pushing image targets
# Image URL to use all building/pushing image targets
IMG ?= github.com/joeyloman/rancher-fip-manager:latest
WEBHOOK_IMG ?= github.com/joeyloman/rancher-fip-manager-webhook:latest
APISERVER_IMG ?= github.com/joeyloman/rancher-fip-api-server:latest
CLUSTERMANAGER_IMG ?= github.com/joeyloman/rancher-fip-cluster-manager:latest
# Produce CRDs that work back to Kubernetes 1.11 (no pruning).
CRD_OPTIONS ?= "crd:trivialVersions=true,preserveUnknownFields=false"

all: manager

# =================================================================================================
# Development
# =================================================================================================

## Run manager binary against the cluster specified in ~/.kube/config
run: generate
	go run ./cmd/manager/main.go --leader-elect=true

## Run tests
test: generate
	go test -race -v ./pkg/... ./cmd/...

## Run the api-server tests
## (the internal/handlers tests use envtest: install binaries with
##  go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.22 use 1.36.x --bin-dir <dir>
##  and export KUBEBUILDER_ASSETS=<dir>/k8s/1.36.2-linux-amd64)
test-apiserver:
	cd apiserver && go test -race -v ./cmd/... ./internal/... ./pkg/...

## Run the cluster-manager tests
## (see test-apiserver for the envtest binary requirement)
test-clustermanager:
	cd clustermanager && go test -race -v ./cmd/... ./internal/... ./pkg/...

# =================================================================================================
# Build
# =================================================================================================

## Build manager binary
manager: generate
	go build -o bin/rancher-fip-manager cmd/manager/main.go

## Build webhook binary
webhook: generate
	go build -o bin/rancher-fip-manager-webhook cmd/webhook/main.go

## Build the cluster-manager binary
clustermanager:
	cd clustermanager && go build -o ../bin/rancher-fip-cluster-manager ./cmd/cluster-manager

## Build the api-server binary
apiserver:
	cd apiserver && go build -o ../bin/rancher-fip-api-server ./cmd/api-server

## Build the docker image
docker-build: test
	docker build -f build/Dockerfile.manager -t ${IMG} .

## Build the webhook docker image
docker-build-webhook: test
	docker build -f build/Dockerfile.webhook -t ${WEBHOOK_IMG} .

## Build the api-server docker image
docker-build-apiserver:
	docker build -f build/Dockerfile.apiserver -t ${APISERVER_IMG} .

## Build the cluster-manager docker image
docker-build-clustermanager:
	docker build -f build/Dockerfile.clustermanager -t ${CLUSTERMANAGER_IMG} .

## Push the docker image
docker-push:
	docker push ${IMG}

## Push the webhook docker image
docker-push-webhook:
	docker push ${WEBHOOK_IMG}

## Push the api-server docker image
docker-push-apiserver:
	docker push ${APISERVER_IMG}

## Push the cluster-manager docker image
docker-push-clustermanager:
	docker push ${CLUSTERMANAGER_IMG}

# =================================================================================================
# Code Generation
# =================================================================================================

## Generate code
generate:
	./hack/update-codegen.sh

# =================================================================================================
# Deployment
# =================================================================================================

## Install CRDs into a cluster
install:
	kubectl apply -f config/crd

## Deploy controller to the cluster
deploy: install
	kubectl apply -f config/deployment/deployment.yaml

## Undeploy controller from the cluster
undeploy:
	kubectl delete -f config/deployment/deployment.yaml
	kubectl delete -f config/crd/

.PHONY: all run test test-apiserver test-clustermanager manager webhook apiserver clustermanager docker-build docker-build-webhook docker-build-apiserver docker-build-clustermanager docker-push docker-push-webhook docker-push-apiserver docker-push-clustermanager generate install deploy undeploy
