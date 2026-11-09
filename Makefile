GO ?= go
LDFLAGS := -s -w

.PHONY: build build-all clean
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/tsnet-bridge-linux-amd64 .

build-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/tsnet-bridge-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/tsnet-bridge-linux-arm64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/tsnet-bridge-linux-armv7 .

clean:
	rm -rf dist
