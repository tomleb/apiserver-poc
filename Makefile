BUILD := docker buildx build

IMAGE_TAG ?= dev
REGISTRY ?= docker.io/$(USER)

K3D_CLUSTER ?= apiserver-poc
K3D_K3S_VERSION ?= v1.30.2
K3D_VERSION ?= v5.7.2

all: image

k3d:
	mkdir -p bin
	@$(BUILD) \
		--target=k3d \
		--build-arg=K3D_VERSION=$(K3D_VERSION) \
		--output=type=local,dest=bin \
		.

create-cluster: k3d
	@bin/k3d cluster create --image rancher/k3s:$(K3D_K3S_VERSION)-k3s1 $(K3D_CLUSTER)

delete-cluster: k3d
	@bin/k3d cluster delete $(K3D_CLUSTER)

image:
	@$(BUILD) \
		--target=apiserver-poc \
		--output=type=image,name=$(REGISTRY)/apiserver-poc:$(IMAGE_TAG) \
		.

import:
	@bin/k3d image import --cluster $(K3D_CLUSTER) $(REGISTRY)/apiserver-poc:$(IMAGE_TAG)

deploy:
	@$(MAKE) image import
	@kubectl apply -f hack/deployment.yaml
	@kubectl delete pods -l app=apiserver-poc

log:
	@kubectl logs --tail=-1 -l app=apiserver-poc -f

.PHONY: all k3d image import restart log
