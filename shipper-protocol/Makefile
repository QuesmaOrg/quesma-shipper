# Makefile for shipper-protocol -- the wire contract between the shipper and its control plane.
# Its own, beside the shipper's at the repository root and Fleet Manager's; `src/` and
# `fleet-manager/` build against this directory through a replace directive in their go.mod.
#
#   make check     gofmt, vet, the suite, and PROTOCOL.md's generated table
#   make gen       regenerate the authority table in PROTOCOL.md from authority.json
#
# Run from anywhere: make -C shipper-protocol help

HERE := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
GO   := CGO_ENABLED=0 go
IN   := cd $(HERE) &&

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@out="$$($(IN) gofmt -l .)"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(IN) $(GO) vet ./...

.PHONY: test
test: ## Run the suite
	$(IN) $(GO) test ./...

.PHONY: doc-check
doc-check: ## Fail when PROTOCOL.md's authority table is stale
	$(IN) $(GO) run ./cmd/protocolgen -check

.PHONY: gen
gen: ## Rewrite PROTOCOL.md's authority table from authority.json
	$(IN) $(GO) run ./cmd/protocolgen

.PHONY: check
check: fmt-check vet test doc-check ## The commit gate: formatting, vet, the suite, the generated table
