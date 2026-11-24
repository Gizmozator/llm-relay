# llm-relay — every build/run/test entry point lives here.
# Recipe lines below use real tab characters; keep them that way.

IMAGE    ?= gitea.rantaeskola.fi/gizmoz/llm-relay:latest
NAME     ?= llm-relay
PORT     ?= 8080
ADMIN_PORT ?= 8081
BIN      ?= llm-relay
REGISTRY ?= gitea.rantaeskola.fi/gizmoz/llm-relay
VERSION  ?= latest

# Export so child processes see these. Make variables are NOT exported by
# default, and `docker compose` is a child process rather than a sub-make:
# without this, `make docker-up PORT=9100` is silently ignored and the compose
# default is used instead.
export PORT
export ADMIN_PORT
export IMAGE

.DEFAULT_GOAL := help

# ------------------------------------------------------------------ local

help: ## list all targets
	@awk 'BEGIN{FS=":.*?## "} /^[a-zA-Z0-9_.-]+:.*## /{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## build the static binary -> $(BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/$(BIN)

# Shared design system (base-styles repo). web/src/base.css is vendored and
# committed; refresh with `make css`. The Docker web stage copies it from the
# build context, so no network access to Gitea is needed there.
BASE_STYLES_TAG ?= v1.0.0
BASE_STYLES_HOST ?= gitea.rantaeskola.fi
BASE_STYLES_URL := http://$(BASE_STYLES_HOST)/gizmoz/base-styles/raw/tag/$(BASE_STYLES_TAG)

# Public-repo rule (AGENTS.md): the upstream header names the bare home domain;
# re-apply the scrubbed provenance line after every fetch so a refresh never
# reintroduces it (matches the scrub in 3afb222). Fetch+scrub+validate run on a
# temp file so a failed gate leaves the committed copy untouched. Two gates:
# the scrub must have matched, and the fetched file must not name the bare
# home host (derived from BASE_STYLES_HOST — the bare form is never spelled
# in this file).
css: ## Fetch shared base.css from base-styles
	curl -fsSL $(BASE_STYLES_URL)/base.css -o web/src/base.css.tmp
	sed 's|^   Shared design-system core.*$$|   Shared design-system core, vendored from the base-styles repo.|' web/src/base.css.tmp > web/src/base.css.new
	@grep -q 'vendored from the base-styles repo' web/src/base.css.new || { rm -f web/src/base.css.tmp web/src/base.css.new; echo 'css: provenance scrub did not match the fetched header — update the sed rule'; exit 1; }
	@! grep -q '$(BASE_STYLES_HOST:gitea.%=%)' web/src/base.css.new || { rm -f web/src/base.css.tmp web/src/base.css.new; echo 'css: fetched file still names the home host — scrub missed'; exit 1; }
	@mv web/src/base.css.new web/src/base.css && rm -f web/src/base.css.tmp

web: css ## build the web dashboard -> web/dist (Docker build does this automatically)
	cd web && npm ci && npm run build

test: ## run unit tests
	go test -race ./...

vet: ## go vet
	go vet ./...

fmt: ## format the tree
	gofmt -w .

fmt-check: ## fail if anything is unformatted
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

lint: vet fmt-check ## static checks (vet + format)

run: build ## run locally: relay on :$(PORT), dashboard on :$(ADMIN_PORT)
	PORT=$(PORT) ADMIN_PORT=$(ADMIN_PORT) ./$(BIN)

watch: build ## rebuild+restart on change (needs air: go install github.com/air-verse/air@latest)
	air

ci: fmt-check vet test ## everything a commit must pass

clean: ## remove build artifacts
	rm -f $(BIN)
	go clean -testcache

# ------------------------------------------------------------------ docker

docker-build: ## build the container image locally for the host arch ($(IMAGE))
	docker build -t $(IMAGE) .

# Multi-arch release needs a container-driver builder (the default `docker`
# driver cannot emit a multi-platform manifest). PLATFORMS is the common
# server/SBC set; override with `make docker-release PLATFORMS=linux/amd64`.
PLATFORMS ?= linux/amd64,linux/arm64
BUILDER   ?= llm-relay-xbuild

dockerx-setup: ## create the multi-arch buildx builder once (docker-container driver)
	docker buildx inspect $(BUILDER) >/dev/null 2>&1 \
	  || docker buildx create --name $(BUILDER) --driver docker-container --bootstrap

docker-release: dockerx-setup ## cross-build $(PLATFORMS) + push multi-arch manifest to the registry (VERSION=x.y.z)
	docker buildx build --builder $(BUILDER) --platform $(PLATFORMS) -t $(REGISTRY):$(VERSION) --push .

docker-up: ## start the stack detached
	docker compose up -d --build

docker-down: ## stop the stack
	docker compose down

docker-logs: ## follow logs
	docker compose logs -f

docker-shell: ## shell in the running container (Go/scratch image: NOT available)
	@echo "NOTE: the Go runtime image is 'scratch' — no sh/bash, so this cannot work."
	@echo "Switch the runtime stage to gcr.io/distroless/base if you need a shell."
	docker exec -it $(NAME) sh || docker exec -it $(NAME) bash

docker-clean: ## stop and remove containers, network, volumes
	docker compose down -v --remove-orphans
