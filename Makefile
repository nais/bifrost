# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/nais/bifrost:main

BUILDTIME = $(shell date "+%s")
DATE = $(shell date "+%Y-%m-%d")
LAST_COMMIT = $(shell git rev-parse --short HEAD)
LDFLAGS := -X github.com/nais/bifrost/pkg/version.Revision=$(LAST_COMMIT) -X github.com/nais/bifrost/pkg/version.Date=$(DATE) -X github.com/nais/bifrost/pkg/version.BuildUnixTime=$(BUILDTIME)

.PHONY: all
all: fmt lint vet check test build

.PHONY: build
build: swagger-generate
	go build -o bin/bifrost -ldflags "-s $(LDFLAGS)" .

.PHONY: swagger-generate
swagger-generate: swag
	$(SWAG) init --parseDependency --parseInternal

.PHONY: test
test:
	go test ./...

.PHONY: start
start:
	go run main.go run

.PHONY: fmt
fmt: gofumpt
	$(GOFUMPT) -w ./

.PHONY: lint
lint: golangci-lint ## Run golangci-lint against code.
	$(GOLANGCI_LINT) run

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: check
check: staticcheck govulncheck
	$(STATICCHECK) ./...
	$(GOVULNCHECK) ./...

.PHONY: docker
docker:
	docker build -t ${IMG} .

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
GOVULNCHECK ?= $(LOCALBIN)/govulncheck
STATICCHECK ?= $(LOCALBIN)/staticcheck
GOFUMPT ?= $(LOCALBIN)/gofumpt
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
SWAG ?= $(LOCALBIN)/swag

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	test -s $(LOCALBIN)/govulncheck || GOBIN=$(LOCALBIN) go install golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: staticcheck
staticcheck: $(STATICCHECK) ## Download staticcheck locally if necessary.
$(STATICCHECK): $(LOCALBIN)
	test -s $(LOCALBIN)/staticcheck || GOBIN=$(LOCALBIN) go install honnef.co/go/tools/cmd/staticcheck@latest

.PHONY: gofumpt
gofumpt: $(GOFUMPT) ## Download gofumpt locally if necessary.
$(GOFUMPT): $(LOCALBIN)
	test -s $(LOCALBIN)/gofumpt || GOBIN=$(LOCALBIN) go install mvdan.cc/gofumpt@latest

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(LOCALBIN)/golangci-lint || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint

.PHONY: swag
swag: $(SWAG) ## Download swag locally if necessary.
$(SWAG): $(LOCALBIN)
	test -s $(LOCALBIN)/swag || GOBIN=$(LOCALBIN) go install github.com/swaggo/swag/cmd/swag@latest
