.PHONY: build release test clean

BINARY := bin/cognix-docker-$(shell uname -s)-$(shell uname -m)

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/cognix-docker

# The binaries the package ships (bin/run.sh runs them; a COGNIX[WS]
# install has no Go). Rebuild them in the commit that changes the Go source.
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/cognix-docker-Linux-x86_64 ./cmd/cognix-docker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/cognix-docker-Linux-aarch64 ./cmd/cognix-docker

test:
	go test -race ./...

clean:
	rm -f bin/cognix-docker-*
