.PHONY: build test fmt lint verify clean deploy

COMMIT := $(shell git describe --always --dirty --tags --match "v*")

build:
	mkdir -p bin
	go build -ldflags "-s -w -X main.version=$(COMMIT) -X github.com/justphantom/miniagent/miniagent.Version=$(COMMIT)" -o bin/miniagent ./cmd/miniagent

test:
	go test -race ./...

fmt:
	gofmt -s -w .

lint:
	golangci-lint run ./...

# verify runs the full AGENTS.md verify-gate: gofmt clean / build ./... / vet / test -race / lint / line-limit.
verify:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt -s -l . (must be empty):"; echo "$$out"; exit 1; fi
	go build ./...
	go vet ./...
	go test -race ./...
	golangci-lint run ./...
	@over=$$(find . -name '*.go' ! -name '*_test.go' -not -path './.git/*' | xargs wc -l | awk '$$1>300 && $$2!="total"{print}'); if [ -n "$$over" ]; then echo "non-test .go files exceeding 300 lines:"; echo "$$over"; exit 1; fi

clean:
	rm -rf bin/

# deploy installs bin/miniagent as a systemd WebUI service (miniagent.service: user miniagent,
# /etc/miniagent/miniagent.json, /var/lib/miniagent, systemctl enable --now).
# Run make build first (dependency deliberately omitted so deploy never rebuilds).
deploy:
	sudo sh deploy/deploy.sh
