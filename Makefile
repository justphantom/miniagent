.PHONY: build test fmt clean deploy

build:
	mkdir -p bin
	go build -o bin/miniagent ./cmd/miniagent

test:
	go test -race ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf bin/

deploy: build
	sudo mv bin/miniagent /usr/local/bin/miniagent
