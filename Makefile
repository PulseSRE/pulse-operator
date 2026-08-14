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
