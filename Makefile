.PHONY: build test check integration doctor systemd-integration benchmark observe static-experiment

build:
	mkdir -p bin
	go build -trimpath -o bin/agentboxd ./cmd/agentboxd
	go build -trimpath -o bin/mockcore ./cmd/mockcore

test:
	go test -race -count=1 ./...

check: test
	go test -tags=integration -run '^TestBenchmarkSummary' -count=1 ./tests/benchmarks
	go vet ./...
	@test -z "$$(gofmt -l cmd internal tests)"
	git diff --check
	bash -n scripts/*.sh
	python3 -c 'import ast,pathlib; [ast.parse(p.read_text(), filename=str(p)) for base in ("scripts", "tests") for p in pathlib.Path(base).rglob("*.py")]'

doctor: build
	./bin/agentboxd -doctor -runsc .local/gvisor/runsc

integration:
	go test -tags=integration -count=1 -v ./tests/integration

systemd-integration: build
	bash scripts/integration-systemd-container.sh

benchmark:
	bash scripts/benchmark-container.sh kvm

observe: build
	bash scripts/observe-container.sh kvm

static-experiment: build
	bash scripts/static-experiment-container.sh kvm
