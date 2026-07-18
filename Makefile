.PHONY: build test fmt clean

build:
	mkdir -p bin
	go build -o bin/miniagent ./cmd/miniagent

test:
	go test -race ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf bin/
