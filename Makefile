# One entry point for both halves of the repo. Anything CI runs, `make check`
# runs locally with the same flags — a CI-only check is a check nobody can fix.

.DEFAULT_GOAL := help
GO  := cd dataplane && go
UV  := cd controlplane && uv

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: check
check: check-go check-py ## Run every check CI runs

.PHONY: check-go
check-go: ## Format check, vet and test the Go data plane
	@test -z "$$(cd dataplane && gofmt -l .)" || { echo "gofmt needed:"; cd dataplane && gofmt -l .; exit 1; }
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...
	@command -v golangci-lint >/dev/null && (cd dataplane && golangci-lint run) \
		|| echo "  (golangci-lint not installed; CI will run it — go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)"


.PHONY: check-py
check-py: ## Lint, type check and test the Python control plane
	$(UV) run ruff check .
	$(UV) run ruff format --check .
	$(UV) run mypy
	$(UV) run pytest

.PHONY: fmt
fmt: ## Apply formatting to both halves
	cd dataplane && gofmt -w .
	$(UV) run ruff format .
	$(UV) run ruff check --fix .

.PHONY: cover
cover: ## Go coverage report
	cd dataplane && go test -coverprofile=coverage.out ./... >/dev/null && go tool cover -func=coverage.out | tail -1
