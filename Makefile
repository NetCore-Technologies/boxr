GO ?= go
VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown
LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: all build test vet fmt-check lint verify clean cross-build

all: test build

build:
	mkdir -p dist
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/boxr ./cmd/boxr

test:
	$(GO) test -race -cover ./...

vet:
	$(GO) vet ./...

fmt-check:
	@test -z "$(shell gofmt -l $$(find . -name '*.go' -not -path './.git/*'))" || { gofmt -l $$(find . -name '*.go' -not -path './.git/*'); exit 1; }

lint: fmt-check vet

verify: lint test cross-build

cross-build:
	@set -eu; \
	for target in linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		out="dist/boxr-$$os-$$arch"; \
		[ "$$os" != windows ] || out="$$out.exe"; \
		mkdir -p dist; echo "Building $$target"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o "$$out" ./cmd/boxr; \
	done

clean:
	rm -rf dist coverage.out coverage.html
