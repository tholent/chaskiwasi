GO ?= go
COMPOSE ?= docker compose -f deploy/compose.dev.yml

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
	$(GO) test -tags e2e ./test/e2e/...

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down -v

clean:
	$(GO) clean -testcache
