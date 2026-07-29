GO ?= go
# DOCKER is a variable so a host whose daemon socket needs elevation can run
# `make e2e DOCKER="sudo docker"`; both the compose commands and the e2e
# harness (via WASI_E2E_DOCKER) then use the same invocation.
DOCKER ?= docker
COMPOSE ?= $(DOCKER) compose -f deploy/compose.dev.yml

.PHONY: build test vet fmt check e2e up down clean

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The gate every wave must pass before the next begins.
check: fmt vet build test

# End-to-end suite (§15): Wasi + strip + maddy, driven by tools/chaskisim.
# Runnable with zero hardware and no Fastmail account.
e2e: up
	WASI_E2E_DOCKER="$(DOCKER)" $(GO) test -tags e2e -timeout 1500s ./test/e2e/...

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down -v

clean:
	$(GO) clean -testcache
