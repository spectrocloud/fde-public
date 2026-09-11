GO      ?= go
GOFLAGS ?= -mod=mod
IMG     ?= docker.io/kreeuwijk/ai-nodeprep
VERSION ?= 0.1.83
PLATFORMS ?= linux/amd64,linux/arm64

# One repo, two binaries: the component rides in the tag because a single
# repository cannot hold two images under the same tag. Bump VERSION here
# together with the image refs in manifests/controller.yaml and agent.yaml.
TAG_CONTROLLER = $(IMG):$(VERSION)-controller
TAG_AGENT      = $(IMG):$(VERSION)-agent

.PHONY: build test vet fmt manifests-install sample clean image-controller image-agent image-multi crd-apply

# Helm ownership markers for manually applied CRDs. Defaults match the
# Palette pack (release "nodeprep", resources in nodeprep-system); if the
# live CRDs carry different values, read them before overwriting:
#   kubectl get crd nodeprepprofiles.nodeprep.spectrocloud.com \
#     -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-namespace}'
HELM_RELEASE ?= nodeprep
HELM_NAMESPACE ?= nodeprep-system

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

CHART_DIR = chart/nodeprep

crd-apply: ## Apply both CRDs and re-stamp Helm's ownership markers
	kubectl apply -f manifests/crd-nodeprep.yaml
	kubectl apply -f manifests/crd-nodeprepprofile.yaml
	@for crd in nodepreps.nodeprep.spectrocloud.com nodeprepprofiles.nodeprep.spectrocloud.com; do \
		kubectl annotate crd $$crd meta.helm.sh/release-name=$(HELM_RELEASE) \
			meta.helm.sh/release-namespace=$(HELM_NAMESPACE) --overwrite && \
		kubectl label crd $$crd app.kubernetes.io/managed-by=Helm --overwrite || exit 1; \
	done

chart-sync-crds: ## Refresh the chart's CRD copies from manifests/ (single source of truth)
	cp manifests/crd-nodeprep.yaml manifests/crd-nodeprepprofile.yaml $(CHART_DIR)/templates/

chart-lint: chart-sync-crds ## helm lint after re-syncing the CRD copies
	helm lint $(CHART_DIR)

chart-template: ## Render the chart (visual check / manifest diffing)
	helm template nodeprep $(CHART_DIR)

chart-package: ## Package the chart into dist/
	mkdir -p dist
	helm package $(CHART_DIR) --destination dist/

clean:
	rm -rf bin/
