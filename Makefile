.PHONY: build test clean

BINARY := bin/cognix-docker-$(shell uname -s)-$(shell uname -m)

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/cognix-docker

test:
	go test -race ./...

clean:
	rm -f bin/cognix-docker-*
