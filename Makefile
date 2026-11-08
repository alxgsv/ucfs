GO ?= go
export CGO_ENABLED = 0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint run cross bench soak

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/ucfs ./cmd/ucfs

test:
	$(GO) test ./...

# The race detector needs cgo. FUSE tests mount only with UCFS_MOUNT_TESTS=1.
race:
	CGO_ENABLED=1 $(GO) test -race ./...

# Benchmarks against internal/fake; the table is Markdown, the raw output
# stays in bin/bench.txt for benchstat.
bench:
	@mkdir -p bin
	$(GO) test -run '^$$' -bench . -benchtime 3x -count 3 ./internal/vfs ./internal/cache | tee bin/bench.txt | $(GO) run ./spikes/benchtable

# Soak: a real mount of a fake project (NFS on macOS, FUSE on Linux) with
# uploads, deletes, metadata changes and readers, sampled into
# bin/soak.csv. SOAK is the duration; see docs/phase5.md.
SOAK ?= 24h
soak:
	@mkdir -p bin
	UCFS_MOUNT_TESTS=1 UCFS_SOAK=$(SOAK) UCFS_SOAK_OUT=$(CURDIR)/bin/soak.csv $(GO) test ./cmd/ucfs -run '^TestSoak$$' -timeout 0 -v

lint:
	golangci-lint run ./...
	GOOS=linux golangci-lint run ./...

run: build
	./bin/ucfs $(ARGS)

cross:
	GOOS=linux GOARCH=amd64 $(GO) build -o /dev/null ./...
	GOOS=linux GOARCH=arm64 $(GO) build -o /dev/null ./...
	GOOS=darwin GOARCH=amd64 $(GO) build -o /dev/null ./...
