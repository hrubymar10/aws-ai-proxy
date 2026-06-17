.PHONY: build test test-race test-full vet cover fmt clean help
.DEFAULT_GOAL := help

build:      ## Build the local binary
	go build -o aws-ai-proxy ./cmd/aws-ai-proxy

test:       ## Unit tests, fast (no race)
	go test ./...

test-race:  ## Tests under the race detector
	go test -race ./...

test-full:  ## Full pre-push gate: vet + race tests
	go vet ./...
	go test -race ./...

vet:        ## go vet
	go vet ./...

cover:      ## Tests with coverage summary
	go test -cover ./...

fmt:        ## Format all Go files
	gofmt -w .

clean:      ## Remove local build artifacts
	rm -f aws-ai-proxy
	rm -f aws-ai-proxy-*

help:       ## List targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-12s %s\n", $$1, $$2}'
