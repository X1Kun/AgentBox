.PHONY: build test check integration doctor

build:
	mkdir -p bin
	go build -trimpath -o bin/agentboxd ./cmd/agentboxd

test:
	go test -race ./...

check: test
	go vet ./...
	@test -z "$$(gofmt -l cmd internal tests)"
	git diff --check
	bash -n scripts/*.sh

doctor: build
	./bin/agentboxd -doctor -runsc .local/gvisor/runsc

integration:
	go test -tags=integration -count=1 -v ./tests/integration
