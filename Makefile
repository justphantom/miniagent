.PHONY: build test fmt lint clean deploy

COMMIT := $(shell git describe --always --dirty --tags)

build:
	mkdir -p bin
	go build -ldflags "-s -w -X main.version=$(COMMIT)" -o bin/miniagent ./cmd/miniagent

test:
	go test -race ./...

fmt:
	gofmt -s -w .

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/

deploy: build
	sudo mv bin/miniagent /usr/local/bin/miniagent
