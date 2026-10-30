.PHONY: build test check integration doctor systemd-integration benchmark observe static-experiment qos-integration platform-preflight platform-create platform-smoke platform-delete platform-gvisor-install platform-gvisor-test platform-network-create platform-network-install platform-network-test
.PHONY: platform-session-build platform-session-install platform-session-test platform-session-warm-test
.PHONY: platform-session-fault-test
.PHONY: platform-api-test platform-api-exec-test

PLATFORM ?= kvm

build:
	mkdir -p bin
	go build -trimpath -o bin/agentboxd ./cmd/agentboxd
	go build -trimpath -o bin/agentbox-gateway ./cmd/agentbox-gateway
	go build -trimpath -o bin/agentbox-mcp ./cmd/agentbox-mcp
	go build -trimpath -o bin/agentbox-agent ./cmd/agentbox-agent
	go build -trimpath -o bin/mockcore ./cmd/mockcore
	go build -trimpath -o bin/agentbox-session-smoke ./cmd/agentbox-session-smoke
	go build -trimpath -o bin/agentbox-api ./cmd/agentbox-api

test:
	go test -race -count=1 ./...

check: test
	go test -tags=integration -run '^TestBenchmarkSummary' -count=1 ./tests/benchmarks
	go vet ./...
	@test -z "$$(gofmt -l cmd internal tests)"
	git diff --check
	bash -n scripts/setup.sh scripts/lib/container-entrypoint.sh scripts/lib/gvisor_node_processes.sh
	python3 -c 'import ast,pathlib; [ast.parse(p.read_text(), filename=str(p)) for base in ("scripts", "tests") for p in pathlib.Path(base).rglob("*.py")]'

doctor: build
	./bin/agentboxd -doctor -runsc .local/gvisor/runsc

integration:
	go test -tags=integration -count=1 -v ./tests/integration

systemd-integration: build
	python3 scripts/container.py systemd --platform $(PLATFORM)

benchmark:
	python3 scripts/container.py benchmark --platform $(PLATFORM)

observe: build
	python3 scripts/container.py observe --platform $(PLATFORM)

static-experiment: build
	python3 scripts/container.py static --platform $(PLATFORM)

qos-integration: build
	python3 scripts/container.py qos --platform $(PLATFORM)

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

# Cached assets and the session image bundle must already be prepared.
CALICO_IMAGE_PREFIX ?= docker.m.daocloud.io/calico
.PHONY: platform-up platform-ready
platform-up:
	python3 scripts/platform_lab.py up --calico-image-prefix $(CALICO_IMAGE_PREFIX)

platform-ready:
	python3 scripts/platform_lab.py ready
