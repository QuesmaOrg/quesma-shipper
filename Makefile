# Makefile for the trajectory shipper.
#
# The Shipper module lives in src/. Its wire contract comes from github.com/QuesmaOrg/shipper-protocol.

MODULE := src
BIN    := bin/shipper
PKG    := ./...

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
RELEASE_VERSION ?=

LDFLAGS :=
ifneq ($(RELEASE_VERSION),)
LDFLAGS += -X github.com/QuesmaOrg/quesma-shipper/internal/platform.releaseVersion=$(RELEASE_VERSION)
endif

GO := CGO_ENABLED=0 go

.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------- build

.PHONY: build
build: ## Build the binary into bin/shipper
	cd $(MODULE) && $(GO) build -ldflags "$(LDFLAGS)" -o ../$(BIN) ./cmd/shipper
	@echo "built $(BIN) ($(VERSION))"

DIST      := bin/dist
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: dist
dist: ## Cross-compile the shipper for every supported platform into bin/dist
	@for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); arch=$$(echo $$p | cut -d/ -f2); \
		out=$(DIST)/shipper-$$os-$$arch; case $$os in windows) out=$$out.exe ;; esac; \
		echo "building $$out"; \
		(cd $(MODULE) && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS) -s -w" -o ../$$out ./cmd/shipper) || exit 1; \
	done
	@echo "built $(DIST)/ ($(VERSION))"

.PHONY: macos-pkg
macos-pkg: ## Build the universal rootless macOS app and pkg (macOS only)
	@test -n "$(RELEASE_VERSION)" || { echo "RELEASE_VERSION is required"; exit 1; }
	src/packaging/macos/pkg/build-pkg.sh "$(RELEASE_VERSION)" "$(DIST)"

.PHONY: install
install: ## Install shipper into GOBIN (or GOPATH/bin)
	cd $(MODULE) && $(GO) install -ldflags "$(LDFLAGS)" ./cmd/shipper

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf bin $(MODULE)/coverage.out $(MODULE)/coverage.html

# ---------------------------------------------------------------------------- preflight

.PHONY: doctor
doctor: ## Check the tools this repository's targets need
	@ok=1; \
	printf '%-10s %-8s %s\n' TOOL STATUS NEEDED-FOR; \
	check() { \
	  if command -v "$$1" >/dev/null 2>&1; then \
	    printf '%-10s %-8s %s\n' "$$1" "ok" "$$3"; \
	  else \
	    printf '%-10s %-8s %s  — %s\n' "$$1" "MISSING" "$$3" "$$4"; \
	    [ "$$2" = required ] && ok=0; \
	  fi; \
	}; \
	check go required "building and testing the client" "https://go.dev/dl"; \
	check claude optional "make cover-review" "https://claude.com/claude-code"; \
	echo; \
	if [ "$$ok" = 1 ]; then echo "ready"; \
	else echo "install what is marked MISSING, then run make doctor again"; exit 1; fi

# ---------------------------------------------------------------------------- test and coverage

.PHONY: test
test: ## Run the unit suite
	cd $(MODULE) && go test $(PKG)

.PHONY: race
race: ## Run the unit suite under the race detector
	cd $(MODULE) && go test -race $(PKG)

# The full local tier measures the shipped binary through a latency-shaped MinIO path. The HTTP
# protocol peer only verifies device signatures and issues presigned tickets; it models no control
# plane product. The full corpus is intentionally manual because it costs minutes.
.PHONY: perf
perf: ## Run the full local performance tier (needs Docker)
	@if ! docker info >/dev/null 2>&1; then \
		echo "docker is not available; skipping the perf tier"; \
		exit 0; \
	fi
	cd $(MODULE) && go vet -tags perf ./perf/
	cd $(MODULE) && go test -tags perf -count=1 -timeout 30m -v ./perf/

# The same instruments and budgets on the PR-sized corpus. The extra three tests prove that the
# proxy and resource observations used by the measured scenarios are live.
.PHONY: perf-smoke
perf-smoke: ## Run the CI-sized performance tier (needs Docker)
	@if ! docker info >/dev/null 2>&1; then \
		echo "docker is not available; skipping the perf smoke tier"; \
		exit 0; \
	fi
	cd $(MODULE) && go vet -tags perf ./perf/
	cd $(MODULE) && go test -tags perf -count=1 -timeout 10m -v -run \
		'^(TestSmokeTier|TestTheStoreIsReachableOnlyThroughTheProxy|TestASyncAgainstABogusEndpointFailsWithoutFallingBack|TestTheHarnessObservesAChildRun)$$' \
		./perf/

.PHONY: cover
cover: ## Write a whole-module coverage report
	cd $(MODULE) && go test -coverpkg=./... -coverprofile=coverage.out \
		$$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...) \
		&& go tool cover -html=coverage.out -o coverage.html
	@cd $(MODULE) && go tool cover -func=coverage.out | tail -1
	@echo "wrote $(MODULE)/coverage.html"

.PHONY: cover-review
cover-review: ## Ask Claude what the coverage gaps actually mean (needs the claude CLI)
	@command -v claude >/dev/null 2>&1 || { \
		echo "needs the claude CLI: https://claude.com/claude-code"; exit 1; }
	@test -f $(MODULE)/coverage.out || $(MAKE) --no-print-directory cover >/dev/null
	@{ \
		echo "# ARCHITECTURE.md"; echo; cat ARCHITECTURE.md; echo; \
		echo "# Per-function coverage of the whole module"; echo; \
		echo "Measured with -coverpkg=./..., so a function shows as covered if ANY test"; \
		echo "reached it — unit, hermetic e2e, or golden."; echo; \
		cd $(MODULE) && go tool cover -func=coverage.out; \
	} | claude -p "$$COVER_REVIEW_PROMPT" | tee coverage-review.md
	@echo; echo "wrote coverage-review.md"

export COVER_REVIEW_PROMPT
define COVER_REVIEW_PROMPT
You are reviewing test coverage for a client that collects AI-agent transcripts from a
developer's machine, redacts secrets, encrypts each file and uploads it to an object store.

Input: the project's ARCHITECTURE.md, then a per-function coverage report for the whole module.

Report ONLY what is worth acting on, ranked by consequence. For each item: the function or
package, what it does, and what would go wrong in production if it is broken — the consequence,
not the percentage. Weigh a gap by what the code is responsible for: anything on the path that
redacts, hashes, names, seals or writes objects is worth ten of anything that formats output or
prints help. Say plainly when an untested function does not matter.

Also name what is covered but only shallowly — a function at 100% whose error paths are never
taken is not tested, and the report cannot show that. Infer it from the architecture and the
function names.

Rules: no praise, no restating the totals, no generic advice about writing more tests. Be
concrete and short. Markdown, at most 400 words, no preamble.
endef

# ---------------------------------------------------------------------------- checks

.PHONY: fmt
fmt: ## Format the module in place
	cd $(MODULE) && gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@cd $(MODULE) && out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet, and type-check the Windows build
	cd $(MODULE) && go vet $(PKG) && GOOS=windows go vet $(PKG)

.PHONY: tidy
tidy: ## Tidy go.mod, failing if it was not already tidy
	cd $(MODULE) && go mod tidy
	@cd $(MODULE) && git diff --quiet go.mod go.sum || \
		{ echo "go.mod/go.sum were not tidy; the fixed files are on disk — commit them"; exit 1; }

.PHONY: version-check
version-check: ## Validate VERSION and construct this commit's release identity
	@sh scripts/release-version.sh >/dev/null

.PHONY: release-version
release-version: ## Print this commit's official release identity
	@sh scripts/release-version.sh

.PHONY: deadcode
deadcode: ## Fail on functions unreachable even with tests as roots
	@cd $(MODULE) && out=$$(GOTOOLCHAIN=$$(go env GOVERSION) go run golang.org/x/tools/cmd/deadcode@v0.49.0 -test ./...); \
	if [ -n "$$out" ]; then echo "$$out"; \
		echo "dead code: delete it, or give it the caller its comment promises"; exit 1; fi

LEGAL_DIR ?= $(MODULE)/internal/legal
GOLICENSES := github.com/google/go-licenses/v2@v2.0.1
LICENSE_OSES := linux darwin windows

.PHONY: licenses-check
licenses-check: ## Fail on a non-permissive dependency, or when the embedded notices are stale
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	cd $(MODULE) && GOTOOLCHAIN=$$(go env GOVERSION) GOBIN=$$tmp/bin go install $(GOLICENSES) 2>/dev/null; \
	for os in $(LICENSE_OSES); do \
		GOOS=$$os GOARCH=amd64 $$tmp/bin/go-licenses check ./cmd/shipper \
			--allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,Unlicense 2>/dev/null || exit 1; \
	done; cd ..; \
	$(MAKE) --no-print-directory licenses LEGAL_DIR=$$tmp/legal >/dev/null || exit 1; \
	if ! diff -r -q -x '*.go' -x references $$tmp/legal $(LEGAL_DIR) >/dev/null; then \
		diff -r -x '*.go' -x references $$tmp/legal $(LEGAL_DIR) | head -20; \
		echo "embedded notices are stale: run make licenses and commit"; exit 1; fi

.PHONY: licenses
licenses: ## Regenerate the embedded notices: LICENSE and NOTICE copies, third-party texts and the inventory for every target OS (references/ is hand-maintained)
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	mkdir -p $(LEGAL_DIR); \
	cd $(MODULE) && GOTOOLCHAIN=$$(go env GOVERSION) GOBIN=$$tmp/bin go install $(GOLICENSES) 2>/dev/null; \
	for os in $(LICENSE_OSES); do \
		GOOS=$$os GOARCH=amd64 $$tmp/bin/go-licenses save ./cmd/shipper --save_path $$tmp/save-$$os 2>/dev/null \
			|| { echo "go-licenses save failed for $$os"; exit 1; }; \
		GOOS=$$os GOARCH=amd64 $$tmp/bin/go-licenses report ./cmd/shipper 2>/dev/null > $$tmp/report-$$os.csv \
			|| { echo "go-licenses report failed for $$os"; exit 1; }; \
	done; \
	cat $$tmp/report-*.csv | grep -v '^github.com/QuesmaOrg/' | LC_ALL=C sort -u > $$tmp/licenses.csv; \
	cd ..; rm -rf $(LEGAL_DIR)/third_party; mkdir -p $(LEGAL_DIR)/third_party/licenses; \
	for os in $(LICENSE_OSES); do \
		chmod -R u+w $(LEGAL_DIR)/third_party; cp -Rf $$tmp/save-$$os/. $(LEGAL_DIR)/third_party/licenses/; \
	done; chmod -R u+w $(LEGAL_DIR)/third_party; \
	rm -rf $(LEGAL_DIR)/third_party/licenses/github.com/QuesmaOrg; \
	cp "$$(cd $(MODULE) && go list -m -f '{{.Dir}}' modernc.org/libc)/LICENSE-3RD-PARTY.md" $(LEGAL_DIR)/third_party/licenses/modernc.org/libc/; \
	cp "$$(cd $(MODULE) && go list -m -f '{{.Dir}}' github.com/creativeprojects/go-selfupdate)/update/LICENSE" $(LEGAL_DIR)/third_party/licenses/github.com/creativeprojects/go-selfupdate/LICENSE.update; \
	mv $$tmp/licenses.csv $(LEGAL_DIR)/third_party/licenses.csv; \
	cp LICENSE NOTICE $(LEGAL_DIR)/; \
	find $(LEGAL_DIR)/third_party -type f -exec chmod 0644 {} +; \
	echo "wrote $(LEGAL_DIR)"

.PHONY: check
check: fmt-check vet version-check deadcode licenses-check race ## Run the commit gate
	@echo "ok"

.PHONY: graph
graph: ## Print the internal import graph
	@cd $(MODULE) && for p in $$(go list ./...); do \
		for d in $$(go list -f '{{join .Imports "\n"}}' $$p | grep '^github.com/QuesmaOrg'); do \
			echo "$${p#github.com/QuesmaOrg/quesma-shipper/} -> $${d#github.com/QuesmaOrg/quesma-shipper/}"; \
		done; \
	done | sort

.PHONY: help
help: ## Show this help
	@echo "trajectory-shipper — $(VERSION)"
	@echo
	@grep -hE '^[a-z][a-z-]*:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'
