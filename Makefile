BIN     ?= token-usage
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)

.PHONY: build test run install uninstall

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/token-usage

test:
	go vet ./... && go test ./...

run: build
	./$(BIN) -listen localhost -port 8787

install: build
	./deploy/install.sh

uninstall:
	./deploy/uninstall.sh
