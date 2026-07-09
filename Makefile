.PHONY: all build build-linux build-linux-all setup clean test vet lint fmt-check ci tidy lima-deps lima-deps-all release-deps

GO := go
# git describe fails with no tags; the trailing sed still exits 0, so fall back
# to "dev" explicitly rather than shipping an empty version.
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//')
VERSION := $(if $(VERSION),$(VERSION),dev)
LDFLAGS := -X github.com/okedeji/agentcage/internal/identity.Version=$(VERSION:v%=%)
GOFLAGS := -trimpath -ldflags '$(LDFLAGS)'
BINDIR := bin

# Lima version + tarball SHA-256s. Single source of truth in
# internal/runtime/lima_release.txt, which the Go runtime embeds for its
# auto-fetch; the Makefile reads the same file so a release build and a runtime
# download pin identical bytes. Bump Lima by editing that file only.
LIMA_PINS := internal/runtime/lima_release.txt
LIMA_VERSION := $(shell awk '$$1=="LIMA_VERSION"{print $$2}' $(LIMA_PINS))
LIMA_SHA256_Darwin_arm64  := $(shell awk '$$1=="LIMA_SHA256_Darwin_arm64"{print $$2}' $(LIMA_PINS))
LIMA_SHA256_Darwin_x86_64 := $(shell awk '$$1=="LIMA_SHA256_Darwin_x86_64"{print $$2}' $(LIMA_PINS))
LIMA_SHA256_Linux_aarch64 := $(shell awk '$$1=="LIMA_SHA256_Linux_aarch64"{print $$2}' $(LIMA_PINS))
LIMA_SHA256_Linux_x86_64  := $(shell awk '$$1=="LIMA_SHA256_Linux_x86_64"{print $$2}' $(LIMA_PINS))

all: vet build

build:
	$(GO) build $(GOFLAGS) -o $(BINDIR)/agentcage ./cmd/agentcage/

# setup builds both binaries a from-source runtime needs: the host CLI and the
# in-VM companion. Lima is fetched by 'agentcage init' on first run, so this is
# the whole from-source setup: `make setup && ./bin/agentcage init`.
setup: build build-linux

# build-linux cross-compiles the agentcage binary that runs inside the VM:
# baked into the gateway image and run by the in-VM daemon. The Lima VM matches
# the host CPU, so the target arch is the host's GOARCH under linux; CGO is off
# so the binary is static and drops into a scratch image. The runtime looks for
# it at bin/agentcage-linux-<arch>, the same bin/ companion layout limactl uses.
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$$($(GO) env GOARCH) $(GO) build $(GOFLAGS) -o $(BINDIR)/agentcage-linux-$$($(GO) env GOARCH) ./cmd/agentcage/

# build-linux-all cross-compiles both in-VM companion arches, the companions a
# release archive ships next to the host binary (one per archive arch). Used by
# release-deps; a dev build only needs the host arch (build-linux).
build-linux-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -o $(BINDIR)/agentcage-linux-arm64 ./cmd/agentcage/
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BINDIR)/agentcage-linux-amd64 ./cmd/agentcage/

clean:
	rm -rf $(BINDIR) dist .lima-release

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
	@if [ -x "$(BINDIR)/lima/bin/limactl" ]; then \
		echo "limactl already at $(BINDIR)/lima/bin/limactl (rm -rf $(BINDIR)/lima to force re-download)"; \
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
	tar -xzf "$$tmpdir/$$tarball" -C "$(BINDIR)/lima" || { echo "lima-deps: extract failed"; exit 1; }; \
	chmod +x "$(BINDIR)/lima/bin/limactl"; \
	echo "installed $(BINDIR)/lima/bin/limactl (lima v$(LIMA_VERSION)) with templates and guest agents"

# lima-deps-all fetches both macOS Lima bundles into .lima-release/lima-darwin-<arch>/,
# the staging dirs the release archives copy from (goreleaser's {{.Os}}-{{.Arch}}
# is darwin-amd64 / darwin-arm64). It stages OUTSIDE dist/ on purpose: goreleaser
# owns dist/ and aborts ("dist is not empty") if a before-hook writes into it.
# Same version pin and SHA verification as lima-deps; it fetches by explicit arch
# rather than uname, so a release built on one Mac still packages both. Lima ships
# macOS only, so Linux archives carry no Lima.
lima-deps-all:
	@set -e; for arch in arm64 amd64; do \
		case "$$arch" in \
			arm64) tarball=lima-$(LIMA_VERSION)-Darwin-arm64.tar.gz;  want=$(LIMA_SHA256_Darwin_arm64) ;; \
			amd64) tarball=lima-$(LIMA_VERSION)-Darwin-x86_64.tar.gz; want=$(LIMA_SHA256_Darwin_x86_64) ;; \
		esac; \
		dest=.lima-release/lima-darwin-$$arch; \
		if [ -x "$$dest/bin/limactl" ]; then echo "lima ($$arch) already at $$dest"; continue; fi; \
		mkdir -p "$$dest"; \
		url=https://github.com/lima-vm/lima/releases/download/v$(LIMA_VERSION)/$$tarball; \
		echo "downloading $$url"; \
		tmpdir=$$(mktemp -d); \
		curl -sSL "$$url" -o "$$tmpdir/$$tarball" || { echo "lima-deps-all: download failed"; rm -rf "$$tmpdir"; exit 1; }; \
		got=$$(shasum -a 256 "$$tmpdir/$$tarball" | awk '{print $$1}'); \
		if [ "$$got" != "$$want" ]; then echo "lima-deps-all: sha256 mismatch for $$arch (got $$got, want $$want)"; rm -rf "$$tmpdir"; exit 1; fi; \
		tar -xzf "$$tmpdir/$$tarball" -C "$$dest" || { echo "lima-deps-all: extract failed"; rm -rf "$$tmpdir"; exit 1; }; \
		chmod +x "$$dest/bin/limactl"; \
		rm -rf "$$tmpdir"; \
		echo "staged $$dest (lima v$(LIMA_VERSION))"; \
	done

# release-deps prepares everything the goreleaser archives copy in: both in-VM
# companion arches and both macOS Lima bundles. Invoked from .goreleaser.yaml's
# before hook.
release-deps: build-linux-all lima-deps-all
