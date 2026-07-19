.PHONY: build test fmt lint clean deploy

COMMIT := $(shell git describe --always --dirty)

build:
	mkdir -p bin
	go build -ldflags "-X main.version=$(COMMIT)" -o bin/miniagent ./cmd/miniagent

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
