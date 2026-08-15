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

## manifests: generate CRD and RBAC manifests via controller-gen
manifests:
	echo "TODO: run controller-gen"

## generate: generate DeepCopy and other generated code via controller-gen
generate:
	echo "TODO: run controller-gen"

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
	oc delete -f deploy/operator.yaml --ignore-not-found=true
	oc delete -f config/crd/bases/pulse.ai_openshiftpulses.yaml --ignore-not-found=true
