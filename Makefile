.PHONY: all build clean test vet lint check-secrets fmt-check ci tidy

GO := go
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -X main.version=$(VERSION:v%=%)
GOFLAGS := -trimpath -ldflags '$(LDFLAGS)'
BINDIR := bin

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

check-secrets:
	$(GO) run scripts/check_secret_redaction.go

fmt-check:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then echo "gofmt: these files are not formatted:" >&2; echo "$$unformatted" >&2; exit 1; fi

ci: fmt-check vet lint check-secrets test build

tidy:
	$(GO) mod tidy
