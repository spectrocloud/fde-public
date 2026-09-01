GO      ?= go
GOFLAGS ?= -mod=mod
IMG     ?= ghcr.io/spectrocloud/nodeprep
TAG     ?= dev

.PHONY: build test vet fmt manifests-install sample clean image image-controller image-agent deploy

build: ## Compile both binaries
	$(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller
	$(GO) build $(GOFLAGS) -o bin/agent ./cmd/agent

test: ## Unit tests
	$(GO) test $(GOFLAGS) ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

fmt:
	$(GO) fmt ./...

image-controller: ## docker build -f Dockerfile --target controller
	docker build -f Dockerfile --target controller -t $(IMG)-controller:$(TAG) .

image-agent:
	docker build -f Dockerfile --target agent -t $(IMG)-agent:$(TAG) .

manifests-install: ## Apply CRDs + RBAC + workloads
	kubectl apply -f manifests/

sample: ## Apply the example profile
	kubectl apply -f samples/profile-gpu-node.yaml

clean:
	rm -rf bin/
