# Makefile for fleet-manager -- the control plane a customer deploys. The verbs every component
# here answers:
#
#   make build                the binary (BIN= to put it elsewhere; the root passes bin/fleet-manager)
#   make run                  serve on the host against the local pipeline's archive bucket, with a
#                             dev organization -- or report that it is already serving
#   make run ENV=<deployment> nothing to run from here: deployments are customers' Terraform
#   make deploy               build the linux/amd64 image and push quesma/fleet-manager:<sha> and
#                             :latest to Docker Hub -- or report that the commit is already there.
#                             No ENV: `latest` is what every customer's Terraform pulls.
#   make setup                create the local bucket and admin credential (idempotent)
#   make check                gofmt, vet, VERSION, dependency licenses, the suite
#   make vulncheck            govulncheck over the module on its own, outside the workspace
#   make licenses             regenerate third_party/ from the module graph
#
# Run from anywhere: make -C fleet-manager help

HERE := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
ROOT := $(abspath $(HERE)..)

GO  := CGO_ENABLED=0 go
IN  := cd $(HERE) &&
ENV ?= local

# The binary lands in THIS directory unless told otherwise; untracked, never committed.
BIN ?= $(HERE)fleet-manager
# The packages are under ./src, which is why the patterns below differ from the other Go modules.
# The UI is embedded in the binary, so asset edits must trigger a rebuild too.
SRC := $(shell find $(HERE)src -type f -not -name '*_test.go') $(HERE)go.mod $(HERE)VERSION

# The reviewed release line, plus the commit that built it. Deliberately not the shipper's
# `VERSION-<commit count>-<sha>`: the count is taken over the whole repository, so it would jump
# discontinuously the day this module is split out into its own. The sha does not.
VERSION  := $(shell tr -d '\r\n' < $(HERE)VERSION)
REVISION := $(shell cd $(HERE) && git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -X main.version=$(VERSION)+$(REVISION)

IMAGE ?= docker.io/quesma/fleet-manager

.DEFAULT_GOAL := help

$(BIN): $(SRC)
	$(IN) $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./src

.PHONY: build
build: $(BIN) ## Build the binary

.PHONY: clean
clean: ## Remove the binary
	rm -f $(BIN)

# Locally it is a HOST process, not a container, and the presigned ticket is why: its signature
# covers the host the client will reach, and the client -- the shipper -- is on the host. A service
# inside a container network would sign for a name the shipper cannot resolve.
#
# Two worlds share this verb. Inside this monorepo, local-pipeline owns the local stack and wires
# the same service into the ETL, so `run` hands over to it. Outside -- a clone of this module on its
# own, which is the open-source case -- there is no local-pipeline, and `run` is the whole thing: a
# MinIO it starts, a bucket, a credential, and the server.
# S3_ENDPOINT names the store. Left at its default it is the loopback MinIO below; set EMPTY it is
# real AWS, which means the standard credential chain -- AWS_PROFILE, SSO, an instance role -- and
# no endpoint override anywhere, rather than the loopback keys. Everything else is the same API.
#
#   make run                                                    the local MinIO
#   AWS_PROFILE=acme make run S3_ENDPOINT= S3_REGION=eu-central-1 DEV_BUCKET=acme-trajectories
#
# What setup provisioned, so run serves the same store without repeating the variables. Included
# before the defaults below: ?= leaves an already-set value alone, and a command-line variable
# overrides both. Delete data/ to start again.
DEV_DIR       := $(HERE)data
-include $(DEV_DIR)/config.mk

S3_ENDPOINT   ?= http://127.0.0.1:9000
S3_ACCESS_KEY ?= localadmin
S3_SECRET_KEY ?= localadmin-secret
S3_REGION     ?= us-east-1
DEV_BUCKET    ?= trajectories
DEV_PORT      ?= 8099

# setup starts the store as well as provisioning it, but only for the default loopback endpoint:
# that is the one store this Makefile can be said to own. Point S3_ENDPOINT anywhere else -- another
# host, a remote MinIO -- and it is yours to run.
MINIO_CONTAINER ?= fleet-minio
MINIO_IMAGE     ?= quay.io/minio/minio
MINIO_MANAGED   := $(if $(filter http://127.0.0.1:9000,$(S3_ENDPOINT)),1,)
DEV_CRED      := $(DEV_DIR)/admin-credential
# The same path without $(HERE), for printing: an absolute path in the output is noise that differs
# on every machine, and the documented sample output would never match it.
DEV_CRED_REL  := $(patsubst $(HERE)%,%,$(DEV_CRED))
ifeq ($(strip $(S3_ENDPOINT)),)
# Real AWS: touch neither the credentials nor the endpoint, so whatever the caller has configured
# is what is used. The service reads the same chain.
DEV_S3        := aws --region $(S3_REGION)
SERVE_ENV     :=
STORE_DESC    := AWS S3 in $(S3_REGION)
else
DEV_AWS       := AWS_ACCESS_KEY_ID=$(S3_ACCESS_KEY) AWS_SECRET_ACCESS_KEY=$(S3_SECRET_KEY) AWS_REGION=$(S3_REGION)
DEV_S3        := $(DEV_AWS) aws --endpoint-url $(S3_ENDPOINT)
SERVE_ENV     := $(DEV_AWS) AWS_ENDPOINT_URL_S3=$(S3_ENDPOINT)
STORE_DESC    := $(S3_ENDPOINT)
endif

.PHONY: run
run: ## Serve on 127.0.0.1:8099 against the local store, setting it up first if needed
ifeq ($(ENV),local)
	@if [ -d $(ROOT)/local-pipeline ]; then \
	  $(MAKE) -C $(ROOT)/local-pipeline start-fleet-manager; \
	else \
	  $(MAKE) --no-print-directory build setup; \
	  printf '  admin UI     http://127.0.0.1:%s/admin/\n' "$(DEV_PORT)"; \
	  printf '\n  Next: open the admin UI, paste the credential, and create an organization with two\n'; \
	  printf '  age recipients (age-keygen -o first.agekey && age-keygen -y first.agekey).\n'; \
	  printf '  Ctrl-C stops the server; the bucket and the credential survive.\n\n'; \
	  $(SERVE_ENV) PORT=$(DEV_PORT) \
	    $(BIN) --serve --provider aws --bucket $(DEV_BUCKET) --region $(S3_REGION); \
	fi
else
	@echo "fleet-manager is deployed into a customer's cloud by the Terraform under terraform/aws"
	@echo "and terraform/gcp. Nothing to run from here."
	@exit 1
endif

# setup provisions, run serves. Both idempotent, so `make setup && make build && make run` and
# `make run` on its own both work.
#
# Inside this monorepo local-pipeline owns the local stack and wires the same service into the ETL,
# against its own MinIO on another port, so run hands over to it. Outside -- a clone of this module
# on its own, which is the open-source case -- there is no local-pipeline and run serves directly.
# That branch is a bridge: it should go in the commit that moves this module out.

.PHONY: setup
setup: ## Create the bucket, turn on versioning, and mint the admin credential
	@for t in aws openssl; do command -v $$t >/dev/null \
	  || { echo "needs $$t on PATH"; exit 1; }; done
	@if [ -z "$(strip $(S3_ENDPOINT))" ] && [ "$(DEV_BUCKET)" = "trajectories" ]; then \
	  printf 'refusing the default bucket name against real AWS: it is not yours and may not be\n'; \
	  printf 'unique. Name it, and check which account you are pointed at first:\n\n'; \
	  printf '  aws sts get-caller-identity\n'; \
	  printf '  make setup S3_ENDPOINT= S3_REGION=<region> DEV_BUCKET=<globally-unique-name>\n'; \
	  exit 1; fi
	@if [ -n "$(MINIO_MANAGED)" ] && ! $(DEV_S3) s3 ls >/dev/null 2>&1; then \
	  command -v docker >/dev/null || { echo "needs docker to start $(MINIO_CONTAINER)"; exit 1; }; \
	  if docker inspect $(MINIO_CONTAINER) >/dev/null 2>&1; then \
	    printf '  starting the existing %s container\n' "$(MINIO_CONTAINER)"; \
	    docker start $(MINIO_CONTAINER) >/dev/null; \
	  else \
	    printf '  starting %s from %s\n' "$(MINIO_CONTAINER)" "$(MINIO_IMAGE)"; \
	    docker run -d --name $(MINIO_CONTAINER) \
	      -p 127.0.0.1:9000:9000 -p 127.0.0.1:9001:9001 \
	      -e MINIO_ROOT_USER=$(S3_ACCESS_KEY) -e MINIO_ROOT_PASSWORD=$(S3_SECRET_KEY) \
	      $(MINIO_IMAGE) server /data --console-address ":9001" >/dev/null; \
	  fi; \
	  n=0; \
	  while ! $(DEV_S3) s3 ls >/dev/null 2>&1; do \
	    n=`expr $$n + 1`; \
	    if [ $$n -gt 30 ]; then echo "  $(MINIO_CONTAINER) did not come up"; exit 1; fi; \
	    sleep 1; \
	  done; \
	fi
	@$(DEV_S3) s3 ls >/dev/null 2>&1 || { \
	  if [ -z "$(strip $(S3_ENDPOINT))" ]; then \
	    printf 'cannot reach AWS S3. Check the credential chain:\n\n'; \
	    printf '  aws sts get-caller-identity\n'; \
	    printf '  AWS_PROFILE=<profile> make setup S3_ENDPOINT= S3_REGION=<region> DEV_BUCKET=<name>\n'; \
	    exit 1; fi; \
	  printf 'no S3 API at %s. Start one:\n\n' "$(S3_ENDPOINT)"; \
	  printf '  docker run -d --name fleet-minio -p 127.0.0.1:9000:9000 -p 127.0.0.1:9001:9001 \\\n'; \
	  printf '    -e MINIO_ROOT_USER=%s -e MINIO_ROOT_PASSWORD=%s \\\n' "$(S3_ACCESS_KEY)" "$(S3_SECRET_KEY)"; \
	  printf '    quay.io/minio/minio server /data --console-address ":9001"\n\n'; \
	  printf 'or point this at another store: S3_ENDPOINT=, S3_ACCESS_KEY=, S3_SECRET_KEY=.\n'; \
	  exit 1; }
	@$(DEV_S3) s3api head-bucket --bucket $(DEV_BUCKET) >/dev/null 2>&1 \
	  || $(DEV_S3) s3 mb s3://$(DEV_BUCKET) >/dev/null
	@$(DEV_S3) s3api put-bucket-versioning --bucket $(DEV_BUCKET) \
	  --versioning-configuration Status=Enabled
	@mkdir -p $(DEV_DIR)
	@if [ ! -f $(DEV_CRED) ]; then umask 077; \
	  printf 'fma1.%s' "$$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')" > $(DEV_CRED); fi
	@cred=$$(cat $(DEV_CRED)); \
	  digest=$$(printf '%s' "$$cred" | openssl dgst -sha256 -r | cut -d' ' -f1); \
	  printf '{"schema":1,"secret_digest":"%s"}' "$$digest" > $(DEV_DIR)/credential.json; \
	  $(DEV_S3) s3 cp $(DEV_DIR)/credential.json \
	    s3://$(DEV_BUCKET)/v1/control/admin/credential.json --only-show-errors
	@printf 'S3_ENDPOINT ?= %s\nS3_REGION ?= %s\nDEV_BUCKET ?= %s\n' \
	  "$(S3_ENDPOINT)" "$(S3_REGION)" "$(DEV_BUCKET)" > $(DEV_DIR)/config.mk
	@printf '\n  storage      %s   bucket %s, versioning on\n' "$(STORE_DESC)" "$(DEV_BUCKET)"
	@printf '  credential   %s\n' "$$(cat $(DEV_CRED))"
	@printf '               kept in %s -- this is the only copy\n\n' "$(DEV_CRED_REL)"

.PHONY: stop
stop: ## Stop the background local process started by local-pipeline (monorepo only)
	@if [ -d $(ROOT)/local-pipeline ]; then $(MAKE) -C $(ROOT)/local-pipeline stop-fleet-manager; \
	else echo "make run serves in the foreground; Ctrl-C stops it."; fi

# amd64 is the one architecture all three deployment targets accept: Cloud Run and Azure Container
# Apps are x86-only, ECS takes either. The cloud-run stage is the plain one; the lambda stage adds
# the web adapter, and nothing pulls it from Docker Hub.
#
# The registry decides what "already deployed" means, not a stamp file -- and only an answer from
# it counts: "not found" means absent, any other failure stops rather than rebuilding over :<sha>.
#   :<sha> there and :latest at the same digest    nothing to do
#   :<sha> there and :latest elsewhere             retag, no rebuild -- how a rollback is spelled
#   :<sha> absent                                  build and push both tags
# A dirty working tree is refused, since :<sha> would name a commit that is not what was built.
# `docker login` is assumed. No provenance or SBOM attestations: they turn the pushed artifact
# into an index that Lambda and some Batch runtimes refuse.
PLATFORM ?= linux/amd64

.PHONY: deploy
deploy: ## Push quesma/fleet-manager:<sha> and :latest to Docker Hub, which every customer's Terraform pulls
	@command -v docker >/dev/null && docker buildx version >/dev/null 2>&1 \
	  || { echo "needs docker with buildx"; exit 1; }
	@if [ -n "$$($(IN) git status --porcelain -- .)" ]; then \
	  echo "the working tree is dirty, so :$(REVISION) would name a commit that is not what gets built:"; \
	  $(IN) git status --short -- . | head -10 | sed 's/^/  /'; exit 1; fi
	@digest() { out=$$(docker buildx imagetools inspect "$$1" --format '{{json .Manifest.Digest}}' 2>&1) \
	  && { printf '%s' "$$out" | tr -d '"'; return 0; }; \
	  case $$out in *": not found"*) return 0 ;; esac; \
	  echo "cannot ask the registry about $$1, so not guessing: $$out" >&2; return 1; }; \
	have=$$(digest $(IMAGE):$(REVISION)) || exit 1; \
	if [ -n "$$have" ]; then \
	  current=$$(digest $(IMAGE):latest) || exit 1; \
	  if [ "$$current" = "$$have" ]; then echo "$(REVISION) is already $(IMAGE):latest. Nothing to do."; exit 0; fi; \
	  docker buildx imagetools create -t $(IMAGE):latest $(IMAGE):$(REVISION); \
	  echo "latest -> $(REVISION), retagged"; exit 0; fi; \
	echo "building $(REVISION) for $(PLATFORM)"; \
	docker buildx build --platform $(PLATFORM) --provenance=false --sbom=false --target cloud-run \
	  --build-arg VERSION=$(VERSION)+$(REVISION) \
	  -t $(IMAGE):$(REVISION) -t $(IMAGE):latest --push $(HERE)

.PHONY: test
test: ## Run the suite
	$(IN) $(GO) test ./src/...

.PHONY: test-ui
test-ui: ## Check browser scripts and date/time handling (Node 22+)
	$(IN) node --check src/admin-ui/app.js
	$(IN) node --check src/admin-ui/date-time.js
	$(IN) node --test tests/*.test.cjs

.PHONY: fmt
fmt: ## Format in place
	$(IN) gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@out="$$($(IN) gofmt -l .)"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(IN) $(GO) vet ./src/...

.PHONY: version-check
version-check: ## Fail unless VERSION holds stable SemVer
	@v="$(VERSION)"; echo "$$v" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$' \
	  || { echo "VERSION must be MAJOR.MINOR.PATCH, got '$$v'"; exit 1; }

# The published image is linux/amd64 and nothing else, so one target is the whole distribution --
# unlike the shipper, which has to ask the question once per operating system it ships a binary for.
#
# go-licenses is installed with GOTOOLCHAIN pinned to the toolchain in use and GOWORK=off, and both
# matter. Built any other way it cannot attribute the standard library when GOROOT lives in the
# module cache, and it then fails with a loader error that looks nothing like a license problem.
GOLICENSES := github.com/google/go-licenses/v2@v2.0.1
ALLOWED    := Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,Unlicense
LEGAL_DIR  ?= $(HERE)third_party

define go_licenses
	GOTOOLCHAIN=$$(cd $(HERE) && go env GOVERSION) GOWORK=off GOBIN=$$tmp/bin \
		go install $(GOLICENSES) 2>/dev/null \
		|| { echo "could not install $(GOLICENSES)"; exit 1; }
endef

.PHONY: licenses-check
licenses-check: ## Fail on a non-permissive dependency, or when third_party/ is stale
	@set -e; tmp=$$(mktemp -d); \
	$(IN) $(go_licenses); \
	$(IN) GOWORK=off GOOS=linux GOARCH=amd64 $$tmp/bin/go-licenses check ./src \
		--allowed_licenses=$(ALLOWED) 2>/dev/null \
		|| { rm -rf "$$tmp"; echo "a dependency's license is outside $(ALLOWED)"; exit 1; }; \
	$(MAKE) --no-print-directory licenses LEGAL_DIR=$$tmp/legal >/dev/null; \
	if ! diff -r -q $$tmp/legal $(LEGAL_DIR) >/dev/null 2>&1; then \
		diff -r $$tmp/legal $(LEGAL_DIR) | head -20; \
		rm -rf "$$tmp"; echo "third_party/ is stale: run make licenses and commit"; exit 1; fi; \
	rm -rf "$$tmp"

.PHONY: licenses
licenses: ## Regenerate third_party/: every dependency's license text, and the inventory
	@set -e; tmp=$$(mktemp -d); \
	$(IN) $(go_licenses); \
	$(IN) GOWORK=off GOOS=linux GOARCH=amd64 $$tmp/bin/go-licenses save ./src \
		--save_path $$tmp/save 2>/dev/null \
		|| { rm -rf "$$tmp"; echo "go-licenses save failed"; exit 1; }; \
	$(IN) GOWORK=off GOOS=linux GOARCH=amd64 $$tmp/bin/go-licenses report ./src 2>/dev/null \
		| grep -v '^github.com/QuesmaOrg/' \
		| awk -F, 'BEGIN{OFS=","}{print $$1,$$3}' | LC_ALL=C sort -u > $$tmp/licenses.csv \
		|| { rm -rf "$$tmp"; echo "go-licenses report failed"; exit 1; }; \
	[ -n "$(LEGAL_DIR)" ] || { echo "LEGAL_DIR is empty"; exit 1; }; \
	rm -rf $(LEGAL_DIR); mkdir -p $(LEGAL_DIR)/licenses; \
	cp -Rf $$tmp/save/. $(LEGAL_DIR)/licenses/; chmod -R u+w $(LEGAL_DIR); \
	rm -rf $(LEGAL_DIR)/licenses/github.com/QuesmaOrg; \
	mv $$tmp/licenses.csv $(LEGAL_DIR)/licenses.csv; \
	find $(LEGAL_DIR) -type f -exec chmod 0644 {} +; \
	rm -rf "$$tmp"; echo "wrote $(LEGAL_DIR)"

.PHONY: check
check: fmt-check vet version-check licenses-check test test-ui ## The commit gate: formatting, vet, VERSION, licenses, Go and UI tests

# GOWORK=off on purpose. Inside a Go workspace this module builds with go.work's toolchain, and
# govulncheck grades the standard library by the module's own `go` directive -- so a scan inside a
# workspace says nothing about what a customer gets when they build the module alone. Not part of
# `check`: a new advisory would fail every unrelated commit, and the scan needs the network.
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: vulncheck
vulncheck: ## govulncheck over the module on its own, outside the workspace
	$(IN) GOWORK=off $(GOVULNCHECK) ./src/...

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
