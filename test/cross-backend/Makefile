# Cross-backend validation harness.
#
#   make validate   # full run: stack up -> wait -> evaluate x3 -> compare
#   make up         # just bring the stack up
#   make evaluate   # run vern evaluate against each backend
#   make compare    # diff the three reports (exits non-zero on disagreement)
#   make down       # tear the stack down + wipe volumes
#   make clean      # down + remove reports/

COMPOSE   := docker compose -f docker-compose.yml
REPO_ROOT := $(abspath ../..)
VERN      := $(REPO_ROOT)/vern

.PHONY: build up down wait-data evaluate compare validate clean

build:
	cd $(REPO_ROOT) && go build -o vern .

up:
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down -v

wait-data:
	bash ./wait-for-data.sh

evaluate: build wait-data
	mkdir -p reports
	cd $(REPO_ROOT) && ./vern evaluate --backend elastic \
	  --config test/cross-backend/vern-configs/elastic.yaml \
	  --output-json test/cross-backend/reports/elastic.json \
	  --output-md   test/cross-backend/reports/elastic.md
	cd $(REPO_ROOT) && ./vern evaluate --backend clickhouse \
	  --config test/cross-backend/vern-configs/clickhouse.yaml \
	  --output-json test/cross-backend/reports/clickhouse.json \
	  --output-md   test/cross-backend/reports/clickhouse.md
	cd $(REPO_ROOT) && ./vern evaluate --backend opensearch \
	  --config test/cross-backend/vern-configs/opensearch.yaml \
	  --output-json test/cross-backend/reports/opensearch.json \
	  --output-md   test/cross-backend/reports/opensearch.md

compare: build
	cd $(REPO_ROOT) && ./vern compare \
	  --label elastic=test/cross-backend/reports/elastic.json \
	  --label clickhouse=test/cross-backend/reports/clickhouse.json \
	  --label opensearch=test/cross-backend/reports/opensearch.json \
	  --expected-gap opensearch:MET-001 \
	  --expected-gap opensearch:MET-006 \
	  --output test/cross-backend/reports/compare.md

validate:
	$(MAKE) up
	$(MAKE) evaluate
	$(MAKE) compare

clean: down
	rm -rf reports
