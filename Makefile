IMG ?= ghcr.io/lhns/kube-oci-composer:latest
CHART_REGISTRY ?= oci://ghcr.io/lhns/charts

CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
CONTROLLER_GEN_VERSION ?= v0.19.0
ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest
ENVTEST_VERSION ?= release-0.21
ENVTEST_K8S_VERSION ?= 1.33.0

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)

.PHONY: all
all: build

##@ Development

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum.
	go mod tidy

.PHONY: fmt
fmt: ## Format the source.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: fmt vet ## Run unit tests. No cluster required.
	go test ./... -race -coverprofile=cover.out

.PHONY: envtest
envtest: ## Install setup-envtest if missing.
	@command -v $(ENVTEST) >/dev/null 2>&1 || \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: integration-test
integration-test: envtest ## Run envtest integration tests against a real API server.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./... -tags=integration -count=1

.PHONY: lint
lint: ## Run golangci-lint if available.
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || \
		echo "golangci-lint not installed; skipping"

##@ Codegen

.PHONY: controller-gen
controller-gen: ## Install controller-gen if missing.
	@command -v $(CONTROLLER_GEN) >/dev/null 2>&1 || \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: manifests
manifests: controller-gen ## Regenerate CRDs, RBAC and deepcopy functions.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/v1alpha1/..."
	$(CONTROLLER_GEN) crd paths="./api/v1alpha1/..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./internal/controller/..." \
		output:rbac:artifacts:config=config/rbac

.PHONY: chart-crds
chart-crds: manifests ## Copy generated CRDs into the chart.
	cp config/crd/bases/*.yaml charts/kube-oci-composer/crds/

.PHONY: schemas
schemas: manifests ## Publish JSON schemas for kubeconform.
	@mkdir -p schemas
	go run ./hack/schemagen config/crd/bases schemas

.PHONY: generate
generate: manifests chart-crds schemas ## Run every generator.

.PHONY: verify
verify: generate ## Fail if generated output is not committed.
	@git diff --exit-code || \
		{ echo "generated files are out of date; run 'make generate' and commit"; exit 1; }

##@ Build

.PHONY: build
build: ## Build the manager binary.
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/manager ./cmd/oci-composer

.PHONY: run
run: manifests ## Run the controller against the current kubecontext.
	go run ./cmd/oci-composer --serving-host=localhost:5000

.PHONY: docker-build
docker-build: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the container image.
	docker push $(IMG)

##@ Deploy

.PHONY: install
install: manifests ## Install CRDs into the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: ## Remove CRDs from the current cluster.
	kubectl delete --ignore-not-found -f config/crd/bases

.PHONY: chart-lint
chart-lint: ## Lint and render the chart.
	helm lint charts/kube-oci-composer
	helm template kube-oci-composer charts/kube-oci-composer >/dev/null

.PHONY: chart-push
chart-push: ## Package and push the chart as an OCI artifact.
	helm package charts/kube-oci-composer --destination bin/
	helm push bin/kube-oci-composer-*.tgz $(CHART_REGISTRY)

##@ End to end

.PHONY: e2e-up
e2e-up: ## Create the kind cluster used by the e2e tests.
	./test/e2e/up.sh

.PHONY: e2e-test
e2e-test: ## Run the e2e tests against the current cluster.
	go test ./test/e2e/... -tags=e2e -timeout 15m -count=1 -v

.PHONY: e2e-down
e2e-down: ## Delete the kind cluster.
	./test/e2e/down.sh

.PHONY: e2e
e2e: e2e-up e2e-test e2e-down ## Full e2e cycle.

##@ Help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
