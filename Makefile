# llm-relay — every build/run/test entry point lives here.
# Recipe lines below use real tab characters; keep them that way.

IMAGE    ?= llm-relay:latest
NAME     ?= llm-relay
PORT     ?= 8080
BIN      ?= llm-relay

# Export so child processes see these. Make variables are NOT exported by
# default, and `docker compose` is a child process rather than a sub-make:
# without this, `make docker-up PORT=9100` is silently ignored and the compose
# default is used instead.
export PORT
export IMAGE

.DEFAULT_GOAL := help

# ------------------------------------------------------------------ local

help: ## list all targets
	@awk 'BEGIN{FS=":.*?## "} /^[a-zA-Z0-9_.-]+:.*## /{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## build the static binary -> $(BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/$(BIN)

test: ## run unit tests
	go test ./...

vet: ## go vet
	go vet ./...

fmt: ## format the tree
	gofmt -w .

fmt-check: ## fail if anything is unformatted
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

lint: vet fmt-check ## static checks (vet + format)

run: build ## run locally on http://127.0.0.1:$(PORT)
	PORT=$(PORT) ./$(BIN)

watch: build ## rebuild+restart on change (needs air: go install github.com/air-verse/air@latest)
	air

ci: fmt-check vet test ## everything a commit must pass

clean: ## remove build artifacts
	rm -f $(BIN)
	go clean -testcache

# ------------------------------------------------------------------ docker

docker-build: ## build the container image ($(IMAGE))
	docker build -t $(IMAGE) .

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
