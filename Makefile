.PHONY: build release check-release test clean

BINARY := bin/cognix-docker-$(shell uname -s)-$(shell uname -m)

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/cognix-docker

# The binaries the package ships (bin/run.sh runs them; a COGNIX[WS]
# install has no Go). Rebuild them in the commit that changes the Go source.
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/cognix-docker-Linux-x86_64 ./cmd/cognix-docker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/cognix-docker-Linux-aarch64 ./cmd/cognix-docker

# Proves the shipped binaries are this source's: rebuilds them into
# .release-check/ (git-ignored; -trimpath builds are reproducible with the
# same Go) and compares byte for byte.
check-release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o .release-check/x86_64 ./cmd/cognix-docker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o .release-check/aarch64 ./cmd/cognix-docker
	cmp .release-check/x86_64 bin/cognix-docker-Linux-x86_64
	cmp .release-check/aarch64 bin/cognix-docker-Linux-aarch64
	rm -f .release-check/x86_64 .release-check/aarch64
	@echo "release binaries match the source"

test: check-release
	go test -race ./...

clean:
	rm -f bin/cognix-docker-*
