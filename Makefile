CONTAINER_TOOL ?= podman

.PHONY: build test run fmt vet manifests generate

## build: compile the operator manager binary
build:
	go build -o bin/manager ./cmd/main.go

## test: run all tests with coverage
test:
	go test ./... -coverprofile cover.out

## run: run the operator locally (requires a valid kubeconfig)
run:
	go run ./cmd/main.go

## fmt: reformat all Go source files
fmt:
	gofmt -s -w .

## vet: run go vet on all packages
vet:
	go vet ./...

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.21.0

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Install controller-gen locally if not present
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

## manifests: generate the CRD (config/crd/bases) from api/v1alpha1 markers.
## Does NOT touch config/rbac/role.yaml: this operator's RBAC includes many
## OpenShift-specific groups that are hand-curated rather than derived from
## +kubebuilder:rbac markers (see scripts/sync-rbac.py's docstring) — running
## controller-gen's rbac generator here would silently blow those away.
## After running, also sync bundle/manifests/*.yaml (see the `bundle` target)
## so the two CRD copies don't drift — CI's rbac-and-crd-sync job checks that.
manifests: controller-gen
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd/bases

## generate: regenerate zz_generated.deepcopy.go from api/v1alpha1 types.
generate: controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."

# Bundle targets
BUNDLE_IMG ?= quay.io/amobrem/pulse-operator-bundle:v0.1.0
OPERATOR_IMG ?= quay.io/amobrem/pulse-operator:latest

.PHONY: bundle
bundle: ## Generate bundle manifests (run after any types/RBAC change)
	@echo "Bundle already pre-generated in bundle/ directory"
	@echo "Update bundle/manifests/*.yaml manually or re-run controller-gen"
	cp config/crd/bases/pulse.ai_openshiftpulses.yaml bundle/manifests/

.PHONY: bundle-build
bundle-build: ## Build the bundle image
	$(CONTAINER_TOOL) build -f Dockerfile.bundle -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

.PHONY: bundle-validate
bundle-validate: ## Validate the bundle (requires operator-sdk)
	operator-sdk bundle validate ./bundle --select-optional suite=operatorframework

.PHONY: bundle-run
bundle-run: ## Install bundle on cluster via operator-sdk (for testing)
	operator-sdk run bundle $(BUNDLE_IMG) --namespace operators

.PHONY: docker-build
docker-build: ## Build operator manager image
	$(CONTAINER_TOOL) build --platform linux/amd64 -f Dockerfile -t $(OPERATOR_IMG) .

.PHONY: docker-push
docker-push: ## Push operator manager image
	$(CONTAINER_TOOL) push $(OPERATOR_IMG)

.PHONY: deploy
deploy: ## Deploy operator to cluster (requires kubectl/oc and cluster access)
	oc apply -f config/crd/bases/pulse.ai_openshiftpulses.yaml
	oc apply -f deploy/operator.yaml
	oc rollout status deployment/pulse-operator-manager -n pulse-operator-system --timeout=120s

.PHONY: undeploy
undeploy: ## Remove operator from cluster
	# Delete CR instances (and wait for the pulse.ai/cleanup finalizer to
	# clear) *before* removing the operator. Doing this in the other order
	# leaves nothing running to process the finalizer, so any live CR hangs
	# in Terminating forever, and the CRD delete below then blocks behind it
	# (K8s waits for all instances of a CRD to be gone before removing it).
	oc delete openshiftpulse --all --all-namespaces --wait=true --timeout=120s --ignore-not-found=true
	oc delete -f deploy/operator.yaml --ignore-not-found=true
	oc delete -f config/crd/bases/pulse.ai_openshiftpulses.yaml --ignore-not-found=true

.PHONY: setup-envtest
setup-envtest: ## Install envtest binaries for local test runs
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@echo "Run: export KUBEBUILDER_ASSETS=\$$(setup-envtest use 1.31 -p path)"
	@echo "Then: make test"
