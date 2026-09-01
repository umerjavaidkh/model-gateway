# One entry point for both halves of the repo. Anything CI runs, `make check`
# runs locally with the same flags — a CI-only check is a check nobody can fix.

.DEFAULT_GOAL := help
GO  := cd dataplane && go
UV  := cd controlplane && uv

# Demo only. A real deployment supplies this from a secret manager.
DEMO_PEPPER := local-dev-pepper-not-for-production!!

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: check
check: check-go check-py ## Run every check CI runs

.PHONY: check-go
check-go: ## Format check, vet and test the Go data plane
	@test -z "$$(cd dataplane && gofmt -l .)" || { echo "gofmt needed:"; cd dataplane && gofmt -l .; exit 1; }
	./scripts/check-core-imports.sh
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

.PHONY: proto
proto: ## Regenerate Go code from proto/ (needs protoc and protoc-gen-go)
	protoc --proto_path=proto \
		--go_out=dataplane/internal/wire/gatewayv1 --go_opt=paths=source_relative \
		--go_opt=Mgateway/v1/snapshot.proto=github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1 \
		proto/gateway/v1/snapshot.proto
	mv dataplane/internal/wire/gatewayv1/gateway/v1/snapshot.pb.go dataplane/internal/wire/gatewayv1/
	rm -rf dataplane/internal/wire/gatewayv1/gateway
	# Strip the toolchain version banner. It records which protoc built the
	# file, which differs between a developer's machine and CI and makes the
	# drift check fail on a comment rather than on real drift.
	sed -i.bak -e '/^\/\/ versions:$$/d' -e '/^\/\/[[:space:]]*protoc/d' \
		dataplane/internal/wire/gatewayv1/snapshot.pb.go
	rm -f dataplane/internal/wire/gatewayv1/snapshot.pb.go.bak
	cd dataplane && gofmt -w internal/wire/gatewayv1/snapshot.pb.go

.PHONY: demo
demo: ## Build a demo snapshot and run the gateway on :8080
	@cd dataplane && go run ./cmd/snapshotgen -out ../snapshot.pb -pepper "$(DEMO_PEPPER)" -secret demo-secret
	@echo
	@echo "  curl -s localhost:8080/v1/chat/completions \\"
	@echo "    -H 'Authorization: Bearer gw_demo_demo-secret' \\"
	@echo "    -d '{\"model\":\"fast\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo
	@cd dataplane && GATEWAY_SNAPSHOT_FILE=../snapshot.pb GATEWAY_KEY_PEPPER="$(DEMO_PEPPER)" go run ./cmd/gateway

.PHONY: fmt
fmt: ## Apply formatting to both halves
	cd dataplane && gofmt -w .
	$(UV) run ruff format .
	$(UV) run ruff check --fix .

.PHONY: cover
cover: ## Go coverage report
	cd dataplane && go test -coverprofile=coverage.out ./... >/dev/null && go tool cover -func=coverage.out | tail -1
