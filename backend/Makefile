# Contact-ingest Lambda (Go) — build/test/lint targets.
# Build produces a static arm64 `bootstrap` binary for the provided.al2023 custom runtime;
# terraform/lambda.tf archives backend/bootstrap into the deployment zip.
BINARY := bootstrap
PKG := ./...

.PHONY: build test fmt vet lint tidy clean help

build: ## Build the arm64 bootstrap binary for provided.al2023
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/ingest

test: ## Run all tests
	go test $(PKG)

fmt: ## Format all Go files in place
	gofmt -w .

vet: ## Run go vet
	go vet $(PKG)

lint: ## gofmt check + vet + staticcheck (if installed) — non-mutating
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi
	go vet $(PKG)
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck $(PKG) \
		|| echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"

tidy: ## go mod tidy
	go mod tidy

clean: ## Remove build artifacts
	rm -f $(BINARY)

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'
