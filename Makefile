BINARY := vern
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/ericmustin/vern/cmd.version=$(VERSION)
DIST_DIR ?= dist

.PHONY: build install test clean dist demo-up demo-down demo-review demo-validate spec-status spec-sync semconv-sync cross-backend-up cross-backend-validate cross-backend-down es-test-up es-test-down capture image image-ci ci-test up down score serve

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

# Cross-backend validation harness (test/cross-backend/). Stands up Elastic,
# ClickHouse, and OpenSearch behind one OTel collector, evaluates each, and
# diffs the scores. `cross-backend-validate` exits non-zero on disagreement.
cross-backend-up:
	$(MAKE) -C test/cross-backend up

cross-backend-validate:
	$(MAKE) -C test/cross-backend validate

cross-backend-down:
	$(MAKE) -C test/cross-backend down

# Local Elasticsearch for integration tests (go test -p 1 -tags integration ./...).
# -p 1 because packages share this cluster and its global component template.
# Security off, Basic license, bound to 127.0.0.1:9200 only.
ES_TEST_IMAGE ?= docker.elastic.co/elasticsearch/elasticsearch:9.5.5
ES_TEST_URL ?= http://127.0.0.1:9200

es-test-up:
	docker run -d --name vern-es-test -p 127.0.0.1:9200:9200 \
		-e discovery.type=single-node \
		-e xpack.security.enabled=false \
		-e xpack.license.self_generated.type=basic \
		-e "ES_JAVA_OPTS=-Xms1g -Xmx1g" \
		$(ES_TEST_IMAGE)
	@echo "Waiting for $(ES_TEST_URL)/_cluster/health ..."
	@for i in $$(seq 1 90); do \
		if curl -fsS "$(ES_TEST_URL)/_cluster/health" >/dev/null 2>&1; then \
			echo "Elasticsearch is up."; exit 0; \
		fi; \
		sleep 2; \
	done; \
	echo "Elasticsearch did not answer within 180s" >&2; exit 1

es-test-down:
	docker rm -f vern-es-test

# Capture a vendor-neutral OTLP JSONL sample. The stack publishes no host ports.
# Not --abort-on-container-exit: each generator exits 0 on its own after
# CAPTURE_SECONDS (default 90) once its SDK providers are flushed, then the
# collector is stopped gracefully so the file exporter flushes.
CAPTURE_COMPOSE ?= docker compose -f demo/capture/compose.yaml
capture:
	: > demo/samples/capture.jsonl
	@set -e; \
	trap '$(CAPTURE_COMPOSE) down -v --remove-orphans >/dev/null 2>&1 || true' EXIT; \
	$(CAPTURE_COMPOSE) up -d --build otel-collector; \
	$(CAPTURE_COMPOSE) up -d --build trafficgen metricsgen; \
	rc=0; for n in trafficgen metricsgen; do \
		c=$$(docker wait $$($(CAPTURE_COMPOSE) ps -aq $$n)); \
		echo "$$n exit code $$c"; \
		[ "$$c" = 0 ] || rc=1; \
	done; \
	col=$$($(CAPTURE_COMPOSE) ps -aq otel-collector); \
	$(CAPTURE_COMPOSE) stop -t 30 otel-collector || { echo "capture failed: collector did not stop" >&2; exit 1; }; \
	cc=$$(docker wait $$col); \
	echo "otel-collector exit code $$cc"; \
	[ "$$cc" = 0 ] || { echo "capture failed: collector exited $$cc (file may be unflushed)" >&2; exit 1; }; \
	[ $$rc = 0 ] || { echo "capture failed: a generator exited non-zero" >&2; exit 1; }; \
	$(CAPTURE_COMPOSE) down -v --remove-orphans
	@test -s demo/samples/capture.jsonl || { echo "capture.jsonl is empty" >&2; exit 1; }
	@jq -r 'keys[0]' demo/samples/capture.jsonl | sort | uniq -c

# Container image and compose bundle (deploy/README.md).
COMPOSE ?= docker compose -f deploy/compose.yaml

image:
	docker build -t vern:dev --build-arg VERSION=$(VERSION) .

image-ci:
	docker build --target ci -t vern:dev-ci --build-arg VERSION=$(VERSION) .

ci-test:
	bash ci/test/vern-ci-test.sh
	shellcheck ci/*.sh ci/test/*.sh

# Elasticsearch, vern-init, then vern serve (the UI). Builds vern:dev first.
up: image
	$(COMPOSE) up -d --wait
	@p=$$($(COMPOSE) port vern 8080 2>/dev/null | sed 's/.*://'); echo "UI: http://127.0.0.1:$${p:-8080}"

down:
	$(COMPOSE) down -v

# make score SAMPLE=path/to/sample.jsonl.gz
score:
	@test -n "$(SAMPLE)" || { echo "usage: make score SAMPLE=<file>" >&2; exit 2; }
	@d=$$(cd "$$(dirname -- "$(SAMPLE)")" && pwd) && f=$$(basename -- "$(SAMPLE)") && \
		SAMPLES_DIR="$$d" $(COMPOSE) run --rm vern score "/samples/$$f" --output-md /dev/stdout

# Run the UI and API from source against $$VERN_ELASTICSEARCH_URL, on
# 127.0.0.1:8080 unless VERN_LISTEN_ADDR says otherwise.
serve:
	@test -n "$$VERN_ELASTICSEARCH_URL" || { echo "usage: VERN_ELASTICSEARCH_URL=http://127.0.0.1:9200 make serve" >&2; exit 2; }
	VERN_LISTEN_ADDR=$${VERN_LISTEN_ADDR:-127.0.0.1:8080} go run -ldflags "$(LDFLAGS)" . serve
