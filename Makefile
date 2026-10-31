.PHONY: build tools test check integration platform-agent-test platform-preflight platform-create platform-smoke platform-delete platform-gvisor-install platform-gvisor-test platform-network-create platform-network-install platform-network-test platform-session-build platform-session-install platform-session-test platform-session-warm-test platform-session-fault-test platform-api-test platform-api-exec-test platform-api-retention-test platform-up platform-ready
.PHONY: build tools test check integration platform-agent-test

build:
	mkdir -p bin
	go build -trimpath -o bin/agentbox-api ./cmd/agentbox-api
	go build -trimpath -o bin/agentbox-cleanup ./cmd/agentbox-cleanup
	go build -trimpath -o bin/agentbox-mcp ./cmd/agentbox-mcp
	go build -trimpath -o bin/agentbox-agent ./cmd/agentbox-agent

tools:
	mkdir -p bin
	go build -trimpath -o bin/agentbox-session-smoke ./cmd/agentbox-session-smoke
	go build -trimpath -o bin/mockcore ./cmd/mockcore

test:
	go test -race -count=1 ./...

check: test
	go test -race -tags=integration -run '^TestStdio' -count=1 ./tests/integration
	go vet ./...
	@test -z "$$(gofmt -l cmd internal tests)"
	git diff --check
	bash -n scripts/lib/gvisor_node_processes.sh
	python3 -c 'import ast,pathlib; [ast.parse(p.read_text(), filename=str(p)) for base in ("scripts", "tests") for p in pathlib.Path(base).rglob("*.py")]'

integration: platform-agent-test

platform-agent-test:
	python3 scripts/platform_lab.py agent-test

# Functional Kubernetes lab; independent of the legacy daemon build.
platform-preflight:
	python3 scripts/platform_lab.py preflight

platform-create:
	python3 scripts/platform_lab.py create

platform-smoke:
	python3 scripts/platform_lab.py smoke

platform-delete:
	python3 scripts/platform_lab.py delete

platform-gvisor-install:
	python3 scripts/platform_lab.py gvisor-install

platform-gvisor-test:
	python3 scripts/platform_lab.py gvisor-test

platform-network-create:
	python3 scripts/platform_lab.py create --network-profile calico

platform-network-install:
	python3 scripts/platform_lab.py network-install

platform-network-test:
	python3 scripts/platform_lab.py network-test

# Trusted in-cluster Go SDK acceptance; not the product API.
platform-session-build:
	python3 scripts/platform_lab.py session-build

platform-session-install:
	python3 scripts/platform_lab.py session-install

platform-session-test:
	python3 scripts/platform_lab.py session-test

platform-session-warm-test:
	python3 scripts/platform_lab.py session-test --session-profile warm

platform-session-fault-test:
	python3 scripts/platform_lab.py session-test --session-profile fault

platform-api-test:
	python3 scripts/platform_lab.py api-test

platform-api-exec-test:
	python3 scripts/platform_lab.py api-exec-test

platform-api-retention-test:
	python3 scripts/platform_lab.py api-retention-test

# Cached assets and the session image bundle must already be prepared.
CALICO_IMAGE_PREFIX ?= docker.m.daocloud.io/calico
.PHONY: platform-up platform-ready
platform-up:
	python3 scripts/platform_lab.py up --calico-image-prefix $(CALICO_IMAGE_PREFIX)

platform-ready:
	python3 scripts/platform_lab.py ready
