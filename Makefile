# faultline — one command gates everything: `make check`.
GO            ?= go
GOLANGCI_LINT ?= golangci-lint
COVER_MIN     ?= 80
export GOFLAGS ?= -p=3

.PHONY: check fmt fmt-check lint vet test race cover build demo bench bench-quick clean

check: fmt-check vet lint race cover ## format-check + type-check + lint + race tests + coverage gate

fmt: ## rewrite files with gofmt + goimports
	$(GOLANGCI_LINT) fmt ./...

fmt-check: ## fail if any file is not gofmt/goimports clean
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@$(GOLANGCI_LINT) fmt --diff ./... > /dev/null || { echo "goimports needed: run make fmt"; exit 1; }
	@echo "fmt-check: ok"

vet: ## compile everything and run go vet (Go's type-check)
	$(GO) build ./...
	$(GO) vet ./...

lint: ## golangci-lint, zero issues allowed
	$(GOLANGCI_LINT) run ./...

test: ## unit + integration tests
	$(GO) test -count=1 ./...

race: ## tests under the race detector
	$(GO) test -race -count=1 ./...

cover: ## coverage of the internal packages, gated at COVER_MIN percent
	./scripts/coverage.sh $(COVER_MIN)

build: ## build bin/faultline and bin/sampleservice
	$(GO) build -o bin/faultline ./cmd/faultline
	$(GO) build -o bin/sampleservice ./cmd/sampleservice

demo: ## offline end-to-end demo against the bundled sample service
	./scripts/demo.sh

bench: ## injected latency / jitter / bandwidth accuracy (about a minute)
	$(GO) run ./tools/latencybench

bench-quick:
	$(GO) run ./tools/latencybench --quick

clean:
	rm -rf bin out faultline-reports coverage.out
