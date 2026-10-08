GO ?= go
VERSION ?= dev

.PHONY: all build linux-amd64 check clean
all: build

build:
	mkdir -p dist
	$(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o dist/wishlist ./cmd/wishlist

linux-amd64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o dist/wishlist-linux-amd64 ./cmd/wishlist

check:
	$(GO) test -race ./...
	$(GO) vet ./...

clean:
	rm -rf dist
