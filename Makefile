.PHONY: build test fmt lint verify clean deploy

COMMIT := $(shell git describe --always --dirty --tags --match "v*")

build:
	mkdir -p bin
	go build -ldflags "-s -w -X main.version=$(COMMIT)" -o bin/miniagent ./cmd/miniagent

test:
	go test -race ./...

fmt:
	gofmt -s -w .

lint:
	golangci-lint run ./...

# verify runs the full AGENTS.md verify-gate: gofmt clean / build ./... / vet / test -race / lint.
verify:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt -s -l . (must be empty):"; echo "$$out"; exit 1; fi
	go build ./...
	go vet ./...
	go test -race ./...
	golangci-lint run ./...

clean:
	rm -rf bin/

# deploy only installs; run make build first (dependency deliberately omitted so deploy never rebuilds).
deploy:
	sudo install -m 0755 bin/miniagent /usr/local/bin/miniagent
