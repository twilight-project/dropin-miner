GO ?= go
VERSION ?= dev

.PHONY: build test race vet fmt cross tidy verify

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/dropin-miner ./cmd/dropin-miner

test:
	$(GO) test -count=1 ./...

# The race detector needs cgo; deliberately separate from the CGO_ENABLED=0 build.
race:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...
	GOOS=windows GOARCH=amd64 $(GO) vet ./...

fmt:
	$(GO) run golang.org/x/tools/cmd/goimports@v0.30.0 -local github.com/twilight-project/dropin-miner -w .

tidy:
	$(GO) mod tidy

# Every release platform, compile only.
cross:
	@for os in darwin linux windows; do for arch in amd64 arm64; do \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -o /dev/null ./... || exit 1; \
	done; done

verify: build test race vet cross
