# Image URL to use all building/pushing image targets
IMG ?= github.com/joeyloman/rancher-fip-manager:latest
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

# =================================================================================================
# Build
# =================================================================================================

## Build manager binary
manager: generate
	go build -o bin/rancher-fip-manager cmd/manager/main.go

## Build the docker image
docker-build: test
	docker build -f Dockerfile -t ${IMG} .

## Push the docker image
docker-push:
	docker push ${IMG}

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

.PHONY: all run test manager docker-build docker-push generate install deploy undeploy
