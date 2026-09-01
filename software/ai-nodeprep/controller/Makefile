GO      ?= go
GOFLAGS ?= -mod=mod
IMG     ?= docker.io/kreeuwijk/ai-nodeprep
VERSION ?= 0.1.7
PLATFORMS ?= linux/amd64,linux/arm64

# One repo, two binaries: the component rides in the tag because a single
# repository cannot hold two images under the same tag. Bump VERSION here
# together with the image refs in manifests/controller.yaml and agent.yaml.
TAG_CONTROLLER = $(IMG):$(VERSION)-controller
TAG_AGENT      = $(IMG):$(VERSION)-agent

.PHONY: build test vet fmt manifests-install sample clean image-controller image-agent image-multi

build: ## Compile both binaries
	$(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller
	$(GO) build $(GOFLAGS) -o bin/agent ./cmd/agent

test: ## Unit tests
	$(GO) test $(GOFLAGS) ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

fmt:
	$(GO) fmt ./...

image-controller: ## Build the controller image for the local platform
	docker buildx build --load --target controller -t $(TAG_CONTROLLER) .

image-agent:
	docker buildx build --load --target agent -t $(TAG_AGENT) .

image-multi: ## Build + push both images for amd64 and arm64 (needs binfmt for the runtime apk layers; docker login first)
	docker buildx build --platform $(PLATFORMS) --target controller --push -t $(TAG_CONTROLLER) .
	docker buildx build --platform $(PLATFORMS) --target agent --push -t $(TAG_AGENT) .

manifests-install: ## Apply CRDs + RBAC + workloads
	kubectl apply -f manifests/

sample: ## Apply the example profile
	kubectl apply -f samples/profile-gpu-node.yaml

clean:
	rm -rf bin/
