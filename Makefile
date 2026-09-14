# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

GO ?= go

.PHONY: build build-stubs check clean fmt hooks run run-file run-down test-e2e test-postgres

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool
# in go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time: `go tool
# lateregate cover`. The plan: `go tool lateregate list`.
check:
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
SERVICE := luxd
STUBS := lux-stubs
MODULE := $(shell $(GO) list -m)

# Build metadata, deferred so the git and date calls run only for a build. A
# dirty tree marks the commit, because a binary built from uncommitted
# changes cannot be reproduced from its commit.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = $(MODULE)/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT)$(DIRTY) \
          -X $(VERSION_PKG).Date=$(BUILD_DATE)

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	@echo "built $(OUT_DIR)/$(SERVICE)"

# The stubs of spec 015: a provider per dialect, the issuer, the
# authorizer, and the sink, in one binary that never ships in an
# installation.
build-stubs:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -o $(OUT_DIR)/$(STUBS) ./cmd/$(STUBS)
	@echo "built $(OUT_DIR)/$(STUBS)"

# ---------------------------------------------------------------------------
# make run and make run-file: a working gateway on loopback in one command,
# every endpoint luxd dials answered by a stub (spec 015).
#
# The ports are derived from the checkout directory's name, so two
# checkouts do not collide; RUN_PORT overrides the base. Everything the
# stack writes sits under out/run: the pid files run-down reads, the logs,
# the rendered examples, the key encryption key, and the file mode's Key
# value. The recipes need curl beside the toolchain, to mint the token and
# apply the examples.
# ---------------------------------------------------------------------------
RUN_DIR := $(OUT_DIR)/run
EXAMPLES := deploy/examples
RUN_PORT ?= $(shell printf '%s' '$(CURDIR)' | cksum | awk '{ print 20000 + $$1 % 20000 }')
PORT_PUBLIC := $(RUN_PORT)
PORT_INTERNAL := $(shell expr $(RUN_PORT) + 1)
PORT_OPENAI := $(shell expr $(RUN_PORT) + 2)
PORT_ANTHROPIC := $(shell expr $(RUN_PORT) + 3)
PORT_GEMINI := $(shell expr $(RUN_PORT) + 4)
PORT_LUX := $(shell expr $(RUN_PORT) + 5)
PORT_ISSUER := $(shell expr $(RUN_PORT) + 6)
PORT_AUTHORIZER := $(shell expr $(RUN_PORT) + 7)
PORT_SINK := $(shell expr $(RUN_PORT) + 8)
# The public URL names localhost while the stubs sit at 127.0.0.1: the
# loop check of spec 003 compares hostnames alone, so a Provider on the
# gateway's own hostname is refused whatever its port.
LUX_URL := http://localhost:$(PORT_PUBLIC)
INTERNAL_URL := http://127.0.0.1:$(PORT_INTERNAL)
STUB_ISSUER := http://127.0.0.1:$(PORT_ISSUER)
STUB_AUTHORIZER := http://127.0.0.1:$(PORT_AUTHORIZER)
STUB_SINK := http://127.0.0.1:$(PORT_SINK)

# The example manifests name the stubs at fixed ports; a render rewrites
# them to this checkout's. $(1) is the extra sed expression of the mode:
# server mode drops the Key's valueFrom block, which is the file mode's.
STRIP_VALUEFROM := -e '/^  valueFrom:$$/,/^    env: STUB_DEV_KEY$$/d'
define render-examples
	rm -rf $(RUN_DIR)/examples && mkdir -p $(RUN_DIR)/examples && \
	for f in $(EXAMPLES)/*.yaml; do \
	  sed -e 's|127\.0\.0\.1:9101|127.0.0.1:$(PORT_OPENAI)|' \
	      -e 's|127\.0\.0\.1:9102|127.0.0.1:$(PORT_ANTHROPIC)|' \
	      -e 's|127\.0\.0\.1:9103|127.0.0.1:$(PORT_GEMINI)|' \
	      -e 's|127\.0\.0\.1:9104|127.0.0.1:$(PORT_LUX)|' \
	      $(1) "$$f" > "$(RUN_DIR)/examples/$${f##*/}"; \
	done
endef

# wait-for polls a URL with curl until it answers 2xx, for up to thirty
# seconds, and fails naming the log to read. Thirty, not ten: under the
# integration tier's load a stub issuer has taken longer than ten to
# answer its first request, while a process that never comes up is
# reported by its own log.
define wait-for
	i=0; until curl -fsS -o /dev/null "$(1)" 2>/dev/null; do \
	  i=$$((i+1)); \
	  if [ $$i -ge 300 ]; then echo "$(1) did not answer; see $(2)" >&2; exit 1; fi; \
	  sleep 0.1; \
	done
endef

# start-stubs starts lux-stubs on this checkout's ports and waits for the
# issuer, which luxd fetches at start.
define start-stubs
	$(OUT_DIR)/$(STUBS) \
	  -openai-addr 127.0.0.1:$(PORT_OPENAI) -anthropic-addr 127.0.0.1:$(PORT_ANTHROPIC) \
	  -gemini-addr 127.0.0.1:$(PORT_GEMINI) -lux-addr 127.0.0.1:$(PORT_LUX) \
	  -issuer-addr 127.0.0.1:$(PORT_ISSUER) -authorizer-addr 127.0.0.1:$(PORT_AUTHORIZER) \
	  -sink-addr 127.0.0.1:$(PORT_SINK) \
	  > $(RUN_DIR)/stubs.log 2>&1 & echo $$! > $(RUN_DIR)/stubs.pid && \
	$(call wait-for,$(STUB_ISSUER)/.well-known/openid-configuration,$(RUN_DIR)/stubs.log)
endef

# The variables both modes hand luxd: the listeners, the public URL, the
# stub sink, and the admission of loopback upstreams the stubs need.
LUXD_ENV := LUX_PUBLIC_ADDR=127.0.0.1:$(PORT_PUBLIC) LUX_INTERNAL_ADDR=127.0.0.1:$(PORT_INTERNAL) \
	LUX_PUBLIC_URL=$(LUX_URL) \
	LUX_EVENTS_URL=$(STUB_SINK) LUX_EVENTS_SECRET=stub-sink-secret \
	LUX_UPSTREAM_ALLOW_PRIVATE=1

# run: both binaries, the stubs, luxd serve with the memory store, the
# stub issuer, the stub authorizer, and the stub sink, the examples
# applied through /v1 as subject dev, and the three exports printed.
run: build build-stubs
	@set -e; \
	$(MAKE) --no-print-directory run-down; \
	mkdir -p $(RUN_DIR); \
	[ -f $(RUN_DIR)/kek ] || head -c 32 /dev/urandom | base64 | tr -d '\n' > $(RUN_DIR)/kek; \
	$(call render-examples,$(STRIP_VALUEFROM)); \
	$(call start-stubs); \
	$(LUXD_ENV) LUX_SECRETS_KEK=$$(cat $(RUN_DIR)/kek) \
	  LUX_OIDC_ISSUERS=$(STUB_ISSUER) LUX_OIDC_INSECURE_ISSUERS=$(STUB_ISSUER) \
	  LUX_AUTHORIZER_URL=$(STUB_AUTHORIZER) LUX_AUTHORIZER_TOKEN=stub-authorizer-token \
	  $(OUT_DIR)/$(SERVICE) serve > $(RUN_DIR)/luxd.log 2>&1 & echo $$! > $(RUN_DIR)/luxd.pid; \
	$(call wait-for,$(INTERNAL_URL)/readyz,$(RUN_DIR)/luxd.log); \
	TOKEN=$$(curl -fsS -X POST $(STUB_ISSUER)/mint -H 'Content-Type: application/json' -d '{"sub":"dev"}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'); \
	[ -n "$$TOKEN" ] || { echo "the stub issuer minted no token; see $(RUN_DIR)/stubs.log" >&2; exit 1; }; \
	KEY=; \
	for f in $(RUN_DIR)/examples/provider-*.yaml $(RUN_DIR)/examples/model-*.yaml $(RUN_DIR)/examples/budget-*.yaml $(RUN_DIR)/examples/key-*.yaml; do \
	  base=$${f##*/}; base=$${base%.yaml}; kind=$${base%%-*}; name=$${base#*-}; \
	  out=$$(curl -fsS -X PUT "$(LUX_URL)/v1/$${kind}s/$$name" -H "Authorization: Bearer $$TOKEN" -H 'Content-Type: application/yaml' --data-binary @"$$f") \
	    || { echo "applying $$f failed; see $(RUN_DIR)/luxd.log" >&2; exit 1; }; \
	  if [ "$$kind" = key ]; then KEY=$$(printf '%s' "$$out" | sed -n 's/.*"value":"\([^"]*\)".*/\1/p'); fi; \
	done; \
	[ -n "$$KEY" ] || { echo "the Key's value was not returned; see $(RUN_DIR)/luxd.log" >&2; exit 1; }; \
	echo "export LUX_URL=$(LUX_URL)"; \
	echo "export LUX_TOKEN=$$TOKEN"; \
	echo "export LUX_KEY=$$KEY"; \
	echo "# openai SDK: OPENAI_BASE_URL=$(LUX_URL)/openai/v1 OPENAI_API_KEY=\$$LUX_KEY"; \
	echo "# curl -sS $(LUX_URL)/openai/v1/chat/completions -H \"Authorization: Bearer \$$LUX_KEY\" -H 'Content-Type: application/json' -d '{\"model\":\"stub-openai\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'"; \
	echo "# events arrive at $(STUB_SINK)/_events; stop with make run-down"

# run-file: the same stack in the file mode: LUX_MANIFEST_DIR on the
# rendered examples, no issuer and no authorizer, and the Key's value in
# STUB_DEV_KEY, generated once under out/run.
run-file: build build-stubs
	@set -e; \
	$(MAKE) --no-print-directory run-down; \
	mkdir -p $(RUN_DIR); \
	[ -f $(RUN_DIR)/dev-key ] || printf 'lux_%s' "$$(head -c 30 /dev/urandom | base64 | tr '+/' '-_' | tr -d '\n=' | head -c 40)" > $(RUN_DIR)/dev-key; \
	$(call render-examples,); \
	$(call start-stubs); \
	STUB_DEV_KEY=$$(cat $(RUN_DIR)/dev-key) LUX_MANIFEST_DIR=$(RUN_DIR)/examples $(LUXD_ENV) \
	  $(OUT_DIR)/$(SERVICE) serve > $(RUN_DIR)/luxd.log 2>&1 & echo $$! > $(RUN_DIR)/luxd.pid; \
	$(call wait-for,$(INTERNAL_URL)/readyz,$(RUN_DIR)/luxd.log); \
	echo "export LUX_URL=$(LUX_URL)"; \
	echo "export LUX_KEY=$$(cat $(RUN_DIR)/dev-key)"; \
	echo "# file mode: no LUX_TOKEN, the control plane is read-only at $(INTERNAL_URL)/v1 and takes no bearer"; \
	echo "# manifests: $(RUN_DIR)/examples, re-read on SIGHUP; stop with make run-down"

# run-down stops whatever run or run-file started, by the pid files under
# out/run, and waits for each process to exit.
run-down:
	@for p in luxd stubs; do \
	  pidfile=$(RUN_DIR)/$$p.pid; \
	  [ -f "$$pidfile" ] || continue; \
	  pid=$$(cat "$$pidfile"); rm -f "$$pidfile"; \
	  kill "$$pid" 2>/dev/null || continue; \
	  i=0; while kill -0 "$$pid" 2>/dev/null; do \
	    i=$$((i+1)); if [ $$i -ge 150 ]; then kill -9 "$$pid" 2>/dev/null; break; fi; sleep 0.1; \
	  done; \
	  echo "stopped $$p ($$pid)"; \
	done

# The tiers of spec 015 that need more than the toolchain, selected by
# build tag and test name prefix. The package pattern is ./... because the
# postgres tier reaches internal/store as well as test/e2e.
test-e2e:
	$(GO) test -tags=integration -run '^TestE2E' -v ./...

test-postgres:
	@test -n "$$LUX_DB_URL" || { echo "LUX_DB_URL is unset and the postgres tier needs a database: docker run --rm -p 5432:5432 -e POSTGRES_PASSWORD=lux postgres:17-alpine, then LUX_DB_URL=postgres://postgres:lux@127.0.0.1:5432/postgres?sslmode=disable" >&2; exit 1; }
	$(GO) test -tags=postgres -run '^TestPostgres' -v ./...

fmt:
	gofmt -w $$(git ls-files '*.go')

# Point git at the delegating hooks. Per clone, so it is a target.
hooks:
	chmod +x .githooks/*
	git config core.hooksPath .githooks

# Remove the build output.
clean:
	rm -rf $(OUT_DIR)
