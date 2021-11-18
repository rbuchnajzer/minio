PWD := $(shell pwd)
GOPATH := $(shell go env GOPATH)
LDFLAGS := $(shell go run buildscripts/gen-ldflags.go)

GOARCH := $(shell go env GOARCH)
GOOS := $(shell go env GOOS)

VERSION ?= $(shell git describe --tags)
TAG ?= "minio/minio:$(VERSION)"

all: build

checks: ## check dependencies
	@echo "Checking dependencies"
	@(env bash $(PWD)/buildscripts/checkdeps.sh)

help: ## print this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' Makefile | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

getdeps: ## fetch necessary dependencies
	@mkdir -p ${GOPATH}/bin
	@echo "Installing golangci-lint" && curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(GOPATH)/bin v1.43.0
	@echo "Installing msgp" && go install -v github.com/tinylib/msgp@v1.1.7-0.20211026165309-e818a1881b0e
	@echo "Installing stringer" && go install -v golang.org/x/tools/cmd/stringer@latest

crosscompile: ## cross compile minio
	@(env bash $(PWD)/buildscripts/cross-compile.sh)

verifiers: getdeps lint check-gen

check-gen: ## check for updated autogenerated files
	@go generate ./... >/dev/null
	@(! git diff --name-only | grep '_gen.go$$') || (echo "Non-committed changes in auto-generated code is detected, please commit them to proceed." && false)

lint: ## runs golangci-lint suite of linters
	@echo "Running $@ check"
	@GO111MODULE=on ${GOPATH}/bin/golangci-lint cache clean
	@GO111MODULE=on ${GOPATH}/bin/golangci-lint run --build-tags kqueue --timeout=10m --config ./.golangci.yml

check: test
test: verifiers build ## builds minio, runs linters, tests
	@echo "Running unit tests"
	@GO111MODULE=on CGO_ENABLED=0 go test -tags kqueue ./... 1>/dev/null

test-race: verifiers build ## builds minio, runs linters, tests (race)
	@echo "Running unit tests under -race"
	@(env bash $(PWD)/buildscripts/race.sh)

test-iam: build ## verify IAM (external IDP, etcd backends)
	@echo "Running tests for IAM (external IDP, etcd backends)"
	@CGO_ENABLED=0 go test -tags kqueue -v -run TestIAM* ./cmd
	@echo "Running tests for IAM (external IDP, etcd backends) with -race"
	@CGO_ENABLED=1 go test -race -tags kqueue -v -run TestIAM* ./cmd

test-replication: install ## verify multi site replication
	@echo "Running tests for Replication three sites"
	@(env bash $(PWD)/docs/bucket/replication/setup_3site_replication.sh)

verify: ## verify minio various setups
	@echo "Verifying build with race"
	@GO111MODULE=on CGO_ENABLED=1 go build -race -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null
	@(env bash $(PWD)/buildscripts/verify-build.sh)

verify-healing: ## verify healing and replacing disks with minio binary
	@echo "Verify healing build with race"
	@GO111MODULE=on CGO_ENABLED=1 go build -race -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null
	@(env bash $(PWD)/buildscripts/verify-healing.sh)

build: checks ## builds minio to $(PWD)
	@echo "Building minio binary to './minio'"
	@GO111MODULE=on CGO_ENABLED=0 go build -tags kqueue -trimpath --ldflags "$(LDFLAGS)" -o $(PWD)/minio 1>/dev/null

hotfix-vars:
	$(eval LDFLAGS := $(shell MINIO_RELEASE="RELEASE" MINIO_HOTFIX="hotfix.$(shell git rev-parse --short HEAD)" go run buildscripts/gen-ldflags.go $(shell git describe --tags --abbrev=0 | \
    sed 's#RELEASE\.\([0-9]\+\)-\([0-9]\+\)-\([0-9]\+\)T\([0-9]\+\)-\([0-9]\+\)-\([0-9]\+\)Z#\1-\2-\3T\4:\5:\6Z#')))
	$(eval TAG := "minio/minio:$(shell git describe --tags --abbrev=0).hotfix.$(shell git rev-parse --short HEAD)")
hotfix: hotfix-vars install ## builds minio binary with hotfix tags

docker-hotfix: hotfix checks ## builds minio docker container with hotfix tags
	@echo "Building minio docker image '$(TAG)'"
	@docker build -t $(TAG) . -f Dockerfile.dev

docker: build checks ## builds minio docker container
	@echo "Building minio docker image '$(TAG)'"
	@docker build -t $(TAG) . -f Dockerfile.dev

install: build ## builds minio and installs it to $GOPATH/bin.
	@echo "Installing minio binary to '$(GOPATH)/bin/minio'"
	@mkdir -p $(GOPATH)/bin && cp -f $(PWD)/minio $(GOPATH)/bin/minio
	@echo "Installation successful. To learn more, try \"minio --help\"."

clean: ## cleanup all generated assets
	@echo "Cleaning up all the generated files"
	@find . -name '*.test' | xargs rm -fv
	@find . -name '*~' | xargs rm -fv
	@rm -rvf minio
	@rm -rvf build
	@rm -rvf release
	@rm -rvf .verify*
