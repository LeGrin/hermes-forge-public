.DEFAULT_GOAL := help

SONAR_ENV ?= .env.sonar

.PHONY: test
test: ## Run all hermes tests
	go test ./...

.PHONY: test/cov
test/cov: ## Run tests with coverage profile (writes coverage.out)
	go test -coverprofile=coverage.out ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: scan
scan: test/cov ## Run sonar-scanner using local .env.sonar
	test -f $(SONAR_ENV) && set -a && . $(SONAR_ENV) && set +a && \
	cd .. && sonar-scanner -Dsonar.token=$$SONAR_TOKEN

.PHONY: run
run: ## Run hermes locally (HERMES_ADDR to override bind)
	go run ./cmd/hermes

.PHONY: help
help: ## Print this help
	@awk -F '::? .*## ' '/^[^:]+::? .*## / { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
