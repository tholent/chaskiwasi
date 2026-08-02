GO ?= go
# DOCKER is a variable so a host whose daemon socket needs elevation can run
# `make e2e DOCKER="sudo docker"`; both the compose commands and the e2e
# harness (via WASI_E2E_DOCKER) then use the same invocation.
DOCKER ?= docker
COMPOSE ?= $(DOCKER) compose -f deploy/compose.dev.yml

.PHONY: build test vet fmt check e2e up down clean \
        fw-hosttest fw-gates fw-vectors fw-build fw-check fw-bench

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The gate every wave must pass before the next begins.
#
# Firmware host tests and text gates join it because both are cheap and need no
# ESP-IDF (chaski-implementation-plan §1): server-side work must never acquire a
# cross-toolchain dependency, and firmware logic must never go unchecked because
# the hardware toolchain is missing.
check: fmt vet build test fw-hosttest fw-gates

# --- Chaski firmware ---------------------------------------------------------
#
# The split is deliberate. `fw-hosttest` and `fw-gates` run anywhere Go, CMake,
# and a C++ compiler exist. `fw-build` needs ESP-IDF and only proves the target
# image links; nothing about the letter path depends on it (ground rule 3).

HOSTTEST_BUILD ?= build/hosttest

# Logic tier: the sync engine, stores, layout, wipe ordering, token checks —
# everything assertable with no hardware (client §15 host rows).
fw-hosttest:
	cmake -S test/firmware/host -B $(HOSTTEST_BUILD) -G Ninja
	cmake --build $(HOSTTEST_BUILD)
	ctest --test-dir $(HOSTTEST_BUILD) --output-on-failure

# C-15 (vocabulary boundary, address tripwire) and the Unicode-skew check.
# C-16's symbol scan needs a linked ELF and therefore lives in fw-check.
fw-gates:
	$(GO) run ./tools/fwgates strings
	$(GO) run ./tools/fwgates unicode

# Regenerate the two generated testdata sets. Run after any wire change or
# segmenter bump; the outputs are committed.
fw-vectors:
	$(GO) run ./tools/graphvectors
	$(GO) run ./tools/wirefixtures

# Target build. Requires ESP-IDF: source $$IDF_PATH/export.sh first.
fw-build:
	cd firmware/chaski && idf.py build

# The bench tier (client §15's hardware rows). With no board attached every
# test skips with an explanation — a skip is not a pass. See
# test/firmware/bench/README.md for flashing, provisioning and the day-one
# instructions.
#
#   CHASKI_BENCH_PORT=/dev/ttyACM0 make fw-bench
fw-bench:
	$(GO) test -tags bench -v ./test/firmware/bench/

# The full firmware gate, including the C-16 symbol scan over the linked image.
fw-check: fw-hosttest fw-gates fw-build
	$(GO) run ./tools/fwgates symbols firmware/chaski/build/chaski.elf

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
