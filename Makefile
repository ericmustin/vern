BINARY := vern
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/ericmustin/vern/cmd.version=$(VERSION)
DIST_DIR ?= dist

.PHONY: build install test clean dist demo-up demo-down demo-review demo-validate spec-status spec-sync semconv-sync

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install:
	mkdir -p $(BINDIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) .
	@echo "Installed $(BINARY) to $(BINDIR)/$(BINARY)"
	@echo "Make sure $(BINDIR) is on your PATH."

test:
	go test ./...

dist:
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)_$(VERSION)_linux_amd64 .
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)_$(VERSION)_linux_arm64 .
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)_$(VERSION)_darwin_amd64 .
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)_$(VERSION)_darwin_arm64 .
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)_$(VERSION)_windows_amd64.exe .
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)_$(VERSION)_windows_arm64.exe .
	(cd $(DIST_DIR) && shasum -a 256 * > checksums.txt)

clean:
	rm -f $(BINARY)
	rm -rf $(DIST_DIR)

demo-up:
	docker compose -f demo/docker-compose.yml up -d --build

demo-down:
	docker compose -f demo/docker-compose.yml down -v

demo-review:
	go run . --config demo/vern.yaml review

demo-validate:
	go run . --config demo/vern.yaml review --live-es-url http://localhost:9200

# Show drift between local ./spec/ and upstream pinned ref (read-only).
spec-status:
	go run . spec status

# Pull the upstream spec at the pinned ref (writes ./spec/* and ./spec/VERSION).
spec-sync:
	go run . spec sync --apply

# Regenerate internal/semconv/attribute_keys.go and placement.go from upstream.
semconv-sync:
	go run . semconv sync --apply
