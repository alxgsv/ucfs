GO ?= go
export CGO_ENABLED = 0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint run cross

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/ucfs ./cmd/ucfs

test:
	$(GO) test ./...

lint:
	golangci-lint run ./...
	GOOS=linux golangci-lint run ./...

# Release archives, checksums and the Homebrew formula in dist/, built as a
# tag would publish them, without uploading anything; see docs/release.md.
.PHONY: snapshot
snapshot:
	goreleaser release --snapshot --clean --skip=publish

run: build
	./bin/ucfs $(ARGS)

cross:
	GOOS=linux GOARCH=amd64 $(GO) build -o /dev/null ./...
	GOOS=linux GOARCH=arm64 $(GO) build -o /dev/null ./...
	GOOS=darwin GOARCH=amd64 $(GO) build -o /dev/null ./...
