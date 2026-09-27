VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/sumitwaani2/dootd/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

STATICCHECK_VERSION := 2026.2.1

.PHONY: build build-all lint test fmt clean

build: ## Build for the host platform into dist/dootd
	$(GOBUILD) -o dist/dootd ./cmd/dootd

build-all: ## Build static release binaries for linux amd64 + arm64
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o dist/dootd-linux-amd64 ./cmd/dootd
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o dist/dootd-linux-arm64 ./cmd/dootd
	cd dist && sha256sum dootd-linux-amd64 dootd-linux-arm64 > checksums.txt

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

test:
	go test -race ./...

fmt:
	gofmt -w .

clean:
	rm -rf dist
