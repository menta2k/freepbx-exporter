BINARY    := freepbx-exporter
PKG       := github.com/menta2k/freepbx-exporter
CMD       := ./cmd/freepbx-exporter
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GO        ?= go

.PHONY: all build test vet lint cover run clean dist

all: vet test build

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) $(CMD)

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

run: build
	./$(BINARY) -ami.address $${AMI_ADDRESS:-127.0.0.1:5038} \
	            -ami.username $${AMI_USERNAME:-admin} \
	            -ami.secret   $${AMI_SECRET:-secret}

clean:
	rm -f $(BINARY) coverage.out
	rm -rf dist/

dist: build
	mkdir -p dist
	cp $(BINARY) dist/
