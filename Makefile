.PHONY: all build clean test vet lint fmt-check ci tidy lima-deps

GO := go
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -X main.version=$(VERSION:v%=%)
GOFLAGS := -trimpath -ldflags '$(LDFLAGS)'
BINDIR := bin

# Lima bundled with agentcage on macOS / Windows. Update LIMA_VERSION + the
# four pinned SHA-256 sums when bumping.
LIMA_VERSION := 2.1.2
LIMA_SHA256_Darwin_arm64  := 7081d03d01511f20c4a3b38d8120428ef1c66e4b21ec9b54017bc65da60b031f
LIMA_SHA256_Darwin_x86_64 := 3dc5218c7b0cc14126fb6e3ae6f174f026660e4e2cdffcb34b16e5a2f415eb45
LIMA_SHA256_Linux_aarch64 := c2deb0aad9ba375b0d1cc37bf3e01402f2d9ef6cf63db924171d2627823626b0
LIMA_SHA256_Linux_x86_64  := 648ed5f599012a0864bd0c4809063b18116bca57f2593d30547dfedbef3c2ce0

all: vet build

build:
	$(GO) build $(GOFLAGS) -o $(BINDIR)/agentcage ./cmd/agentcage/

clean:
	rm -rf $(BINDIR)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "error: golangci-lint not found (install: https://golangci-lint.run/welcome/install/)"; exit 1; }
	golangci-lint run ./...

fmt-check:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then echo "gofmt: these files are not formatted:" >&2; echo "$$unformatted" >&2; exit 1; fi

ci: fmt-check vet lint test build

tidy:
	$(GO) mod tidy

# lima-deps downloads the limactl binary that ships with agentcage on
# macOS / Windows (no-op on Linux, which uses system containerd directly).
# The release is pinned by version and SHA-256 above so a tampered mirror
# cannot inject a different binary. The extracted limactl ends up at
# bin/lima/limactl, where the runtime package looks for it first before
# falling back to PATH.
lima-deps:
	@mkdir -p $(BINDIR)/lima
	@if [ -x "$(BINDIR)/lima/limactl" ]; then \
		echo "limactl already at $(BINDIR)/lima/limactl (rm to force re-download)"; \
		exit 0; \
	fi; \
	os=$$(uname -s); arch=$$(uname -m); \
	case "$$os-$$arch" in \
		Darwin-arm64)   tarball=lima-$(LIMA_VERSION)-Darwin-arm64.tar.gz;  want=$(LIMA_SHA256_Darwin_arm64) ;; \
		Darwin-x86_64)  tarball=lima-$(LIMA_VERSION)-Darwin-x86_64.tar.gz; want=$(LIMA_SHA256_Darwin_x86_64) ;; \
		Linux-aarch64)  tarball=lima-$(LIMA_VERSION)-Linux-aarch64.tar.gz; want=$(LIMA_SHA256_Linux_aarch64) ;; \
		Linux-x86_64)   tarball=lima-$(LIMA_VERSION)-Linux-x86_64.tar.gz;  want=$(LIMA_SHA256_Linux_x86_64) ;; \
		*) echo "lima-deps: unsupported platform $$os-$$arch"; exit 1 ;; \
	esac; \
	url=https://github.com/lima-vm/lima/releases/download/v$(LIMA_VERSION)/$$tarball; \
	echo "downloading $$url"; \
	tmpdir=$$(mktemp -d); trap "rm -rf $$tmpdir" EXIT; \
	curl -sSL "$$url" -o "$$tmpdir/$$tarball" || { echo "lima-deps: download failed"; exit 1; }; \
	got=$$(shasum -a 256 "$$tmpdir/$$tarball" | awk '{print $$1}'); \
	if [ "$$got" != "$$want" ]; then echo "lima-deps: sha256 mismatch (got $$got, want $$want)"; exit 1; fi; \
	tar -xzf "$$tmpdir/$$tarball" -C "$$tmpdir" || { echo "lima-deps: extract failed"; exit 1; }; \
	cp "$$tmpdir/bin/limactl" "$(BINDIR)/lima/limactl"; \
	chmod +x "$(BINDIR)/lima/limactl"; \
	echo "installed $(BINDIR)/lima/limactl (lima v$(LIMA_VERSION))"
