# Build on the dev Mac, deploy to the host over SSH.
# Host-specific values go in Makefile.local (keep it out of git):
#   DEPLOY_HOST = myhost

-include Makefile.local

DEPLOY_HOST ?= $(error DEPLOY_HOST is not set; add it to Makefile.local)
REMOTE_DIR  ?= /usr/local/libexec/gostafson
SERVICE     ?= system/com.gostafson.agent
KEEP        ?= 5
AGENT_USER  ?= agent
REMOTE_SECRETS ?= /Users/$(AGENT_USER)/secrets.toml
REMOTE_CONFIG  ?= /usr/local/etc/gostafson/config.toml
HOST_CONFIG    ?= config.host.toml
PLIST          ?= /Library/LaunchDaemons/com.gostafson.agent.plist
CONFIG      ?= config.local.toml
TUNNEL_OLLAMA_PORT ?= 11434
TUNNEL_LMS_PORT    ?= 1234

VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
BIN     := dist/gostafson

.PHONY: build test test-integration run tunnel models deploy rollback push-secrets push-config setup versions logs service-log service-status clean check-host

# Fails fast, before test and build, when DEPLOY_HOST is missing.
check-host:
	@: $(DEPLOY_HOST)

build:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
	  -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/gostafson

test:
	go vet ./...
	go test ./...

# Local run with the CLI transport. Pair with `make tunnel` in another terminal.
run:  # make run ARGS=-v for a log line per turn
	go run ./cmd/gostafson -config $(CONFIG) $(ARGS)

# Forward the host's Ollama and LM Studio ports to localhost. Change the local
# ports in Makefile.local if the dev machine runs its own Ollama on 11434.
tunnel:
	ssh -N -L $(TUNNEL_OLLAMA_PORT):localhost:11434 -L $(TUNNEL_LMS_PORT):localhost:1234 $(DEPLOY_HOST)

# Needs the tunnel. Uses the default profile's model on the host.
# Creates gostafson-qwen3.5-9b (qwen3.5:9b with a 32K context) on the host's
# Ollama through the tunnel. Ollama's /v1 API ignores per-request context size
# and defaults to 4096 tokens. Safe to re-run.
models:
	curl -fsS http://localhost:$(TUNNEL_OLLAMA_PORT)/api/create \
	  -d '{"model":"gostafson-qwen3.5-9b","from":"qwen3.5:9b","parameters":{"num_ctx":32768},"stream":false}'
	@echo

test-integration:
	GOSTAFSON_TEST_BASE_URL=http://localhost:$(TUNNEL_OLLAMA_PORT)/v1 go test -tags integration -count=1 ./...

# Copy a versioned binary, point `current` at it, restart, prune old versions.
deploy: check-host test build
	scp $(BIN) $(DEPLOY_HOST):$(REMOTE_DIR)/gostafson-$(VERSION)
	ssh -t $(DEPLOY_HOST) 'set -e; cd $(REMOTE_DIR); \
	  ln -sfn gostafson-$(VERSION) current; \
	  launchctl print $(SERVICE) >/dev/null 2>&1 || sudo launchctl bootstrap system $(PLIST); \
	  sudo launchctl kickstart -k $(SERVICE); \
	  ls -t gostafson-* | tail -n +$$(($(KEEP)+1)) | xargs rm -f; \
	  echo "deployed gostafson-$(VERSION)"'

# Point `current` at the version deployed before the one it points to now.
rollback:
	ssh -t $(DEPLOY_HOST) 'set -e; cd $(REMOTE_DIR); \
	  cur=$$(readlink current); \
	  prev=$$(ls -t gostafson-* | grep -x -A1 "$$cur" | sed -n 2p); \
	  [ -n "$$prev" ] || { echo "no version older than $$cur"; exit 1; }; \
	  ln -sfn "$$prev" current; \
	  sudo launchctl kickstart -k $(SERVICE); \
	  echo "rolled back $$cur -> $$prev"'

# Write the host's secrets file from this shell's environment (bwset
# OPENROUTER_API_KEY, GOSTAFSON_TELEGRAM_TOKEN, GOSTAFSON_WEB_TOKEN; any that
# are set), owned by the agent user with mode 0600. It replaces the whole
# file. Values travel over SSH stdin, never on a command line, and sit briefly
# in a 0600 temp file in the admin account's home on the host.
push-secrets: check-host
	@out=$$(deploy/secrets.sh) || exit 1; \
	  printf '%s\n' "$$out" | ssh $(DEPLOY_HOST) 'umask 077; cat > .gostafson-secrets.tmp'
	@ssh -t $(DEPLOY_HOST) 'sudo install -o $(AGENT_USER) -g $(AGENT_USER) -m 600 .gostafson-secrets.tmp $(REMOTE_SECRETS); rc=$$?; rm -f .gostafson-secrets.tmp; exit $$rc'
	@echo "wrote $(REMOTE_SECRETS); restart to apply: sudo launchctl kickstart -k $(SERVICE)"

# Install config.host.toml as the host's config, owned by root (the agent can
# read it but not change it). Checks first that it has no <placeholders> left.
push-config: check-host
	@test -f $(HOST_CONFIG) || { echo "$(HOST_CONFIG) not found; copy config.host.example.toml and fill it in"; exit 1; }
	@! grep -n '<[^>]*>' $(HOST_CONFIG) || { echo "replace the values in angle brackets above first"; exit 1; }
	scp $(HOST_CONFIG) $(DEPLOY_HOST):.gostafson-config.tmp
	ssh -t $(DEPLOY_HOST) 'sudo install -o root -g wheel -m 644 .gostafson-config.tmp $(REMOTE_CONFIG); rc=$$?; rm -f .gostafson-config.tmp; exit $$rc'
	@echo "installed $(REMOTE_CONFIG); restart to apply: sudo launchctl kickstart -k $(SERVICE)"

# Copy deploy/ to the host and run setup.sh with sudo (see the script's header).
setup: check-host
	ssh $(DEPLOY_HOST) 'rm -rf .gostafson-setup && mkdir .gostafson-setup'
	scp deploy/setup.sh deploy/*.plist $(DEPLOY_HOST):.gostafson-setup/
	ssh -t $(DEPLOY_HOST) 'sudo .gostafson-setup/setup.sh; rc=$$?; rm -rf .gostafson-setup; exit $$rc'

versions:
	ssh $(DEPLOY_HOST) 'cd $(REMOTE_DIR) && ls -lt gostafson-* && echo "current -> $$(readlink current)"'

logs:
	ssh -t $(DEPLOY_HOST) 'sudo tail -f /Users/$(AGENT_USER)/logs/audit.jsonl'

service-log:
	ssh -t $(DEPLOY_HOST) 'sudo tail -n 50 -f /Users/$(AGENT_USER)/logs/gostafson.log /Users/$(AGENT_USER)/logs/wn-agent.log'

service-status:
	@ssh $(DEPLOY_HOST) 'for s in com.gostafson.agent com.gostafson.wn-agent; do \
	  if out=$$(launchctl print system/$$s 2>/dev/null); then \
	    printf "%-24s %s\n" $$s "$$(printf "%s\n" "$$out" | grep -E "^	(state|pid|last exit code) =" | sed "s/^	//" | paste -sd ";" -)"; \
	  else printf "%-24s not loaded\n" $$s; fi; done'

clean:
	rm -rf dist
