BINARY := vern
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/ericmustin/vern/cmd.version=$(VERSION)

.PHONY: build install test clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install:
	mkdir -p $(BINDIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) .
	@echo "Installed $(BINARY) to $(BINDIR)/$(BINARY)"
	@echo "Make sure $(BINDIR) is on your PATH."

test:
	go test ./...

clean:
	rm -f $(BINARY)
