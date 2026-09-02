# One entry point for both halves of the repo. Anything CI runs, `make check`
# runs locally with the same flags — a CI-only check is a check nobody can fix.

.DEFAULT_GOAL := help
GO  := cd dataplane && go
UV  := cd controlplane && uv
NER := cd sidecars/pii-ner && uv

# Where `make wasm-example` puts a built module. Modules are named by their
# own digest, because that is how a manifest refers to one and how a worker
# checks it got the bytes that were admitted.
WASM_DIR := build/wasm

# Demo only. A real deployment supplies this from a secret manager.
DEMO_PEPPER := local-dev-pepper-not-for-production!!

# The floor, enforced in CI. Raise it when the code earns it; never lower it to
# make a red build green.
COVERAGE_THRESHOLD := 80

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: check
check: check-go check-py check-ner cover crosscheck livecheck nercheck admissioncheck ## Run every check CI runs

.PHONY: check-go
check-go: ## Format check, vet and test the Go data plane
	@test -z "$$(cd dataplane && gofmt -l .)" || { echo "gofmt needed:"; cd dataplane && gofmt -l .; exit 1; }
	./scripts/check-core-imports.sh
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...
	@# An if rather than `&& ... || echo`: with the latter, a lint *failure*
	@# also takes the else branch, so real findings were reported as "not
	@# installed" and the target still passed.
	@if command -v golangci-lint >/dev/null; then \
		cd dataplane && golangci-lint run; \
	else \
		echo "  (golangci-lint not installed; CI will run it — go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)"; \
	fi


.PHONY: check-py
check-py: ## Lint, type check and test the Python control plane
	$(UV) run ruff check .
	$(UV) run ruff format --check .
	$(UV) run mypy
	$(UV) run pytest

.PHONY: check-ner
check-ner: ## Lint, type check and test the PII NER sidecar
	$(NER) run ruff check .
	$(NER) run ruff format --check .
	$(NER) run mypy
	$(NER) run pytest

.PHONY: crosscheck
crosscheck: ## Prove the Python builder and the Go worker agree on the snapshot
	./scripts/cross-language-check.sh

.PHONY: livecheck
livecheck: ## Prove configuration reaches a running worker, and survives a control-plane outage
	./scripts/live-subscription-check.sh

.PHONY: admissioncheck
admissioncheck: ## Prove a component cannot be bound until the runner vouches for it
	./scripts/admission-check.sh

.PHONY: wasm-example
wasm-example: ## Build the example WASM guardrail into $(WASM_DIR), named by digest
	@mkdir -p "$(WASM_DIR)"
	@cd examples/wasm-guardrail && GOOS=wasip1 GOARCH=wasm \
		go build -buildmode=c-shared -o "$(CURDIR)/$(WASM_DIR)/module.wasm" .
	@digest=$$(shasum -a 256 "$(WASM_DIR)/module.wasm" | cut -d' ' -f1); \
		mv "$(WASM_DIR)/module.wasm" "$(WASM_DIR)/$$digest.wasm"; \
		echo "sha256:$$digest"

.PHONY: nercheck
nercheck: ## Prove the Go client and the Python sidecar agree about byte offsets
	./scripts/ner-sidecar-check.sh

.PHONY: proto
proto: ## Regenerate Go code from proto/ (needs protoc and protoc-gen-go)
	protoc --proto_path=proto \
		--go_out=dataplane/internal/wire/gatewayv1 --go_opt=paths=source_relative \
		--go_opt=Mgateway/v1/snapshot.proto=github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1 \
		proto/gateway/v1/snapshot.proto proto/gateway/v1/usage.proto
	mv dataplane/internal/wire/gatewayv1/gateway/v1/*.pb.go dataplane/internal/wire/gatewayv1/
	rm -rf dataplane/internal/wire/gatewayv1/gateway
	# Strip the toolchain version banner. It records which protoc built the
	# file, which differs between a developer's machine and CI and makes the
	# drift check fail on a comment rather than on real drift.
	sed -i.bak -e '/^\/\/ versions:$$/d' -e '/^\/\/[[:space:]]*protoc/d' \
		dataplane/internal/wire/gatewayv1/snapshot.pb.go
	rm -f dataplane/internal/wire/gatewayv1/snapshot.pb.go.bak
	cd dataplane && gofmt -w internal/wire/gatewayv1/
	# Python bindings from the same schema, in the same target: generating them
	# separately is how the two halves of a shared contract drift apart.
	protoc --proto_path=proto \
		--python_out=controlplane/src/model_gateway_control/wire \
		--pyi_out=controlplane/src/model_gateway_control/wire \
		proto/gateway/v1/snapshot.proto proto/gateway/v1/usage.proto
	mv controlplane/src/model_gateway_control/wire/gateway/v1/*_pb2.py \
		controlplane/src/model_gateway_control/wire/
	mv controlplane/src/model_gateway_control/wire/gateway/v1/*_pb2.pyi \
		controlplane/src/model_gateway_control/wire/
	rm -rf controlplane/src/model_gateway_control/wire/gateway
	sed -i.bak '/^# Protobuf Python Version:/d' \
		controlplane/src/model_gateway_control/wire/*_pb2.py \
		controlplane/src/model_gateway_control/wire/*_pb2.pyi
	rm -f controlplane/src/model_gateway_control/wire/*.bak

.PHONY: local-up
local-up: ## Build and start the whole fleet in Linux containers, migrated and seeded
	docker compose -f deploy/local/compose.yaml up -d --build
	@echo
	@echo "  worker A  localhost:18080     worker B  localhost:18090"
	@echo "  admin     localhost:18081"
	@echo "  key       gw_demo_local-development-key"
	@echo
	@echo "  make local-smoke   to prove it works"

.PHONY: local-smoke
local-smoke: ## Assert the local fleet behaves like a fleet
	./deploy/local/smoke.sh

.PHONY: local-logs
local-logs: ## Follow every container
	docker compose -f deploy/local/compose.yaml logs -f

.PHONY: local-down
local-down: ## Stop the local fleet and delete its volumes
	docker compose -f deploy/local/compose.yaml down -v

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
fmt: ## Apply formatting across the repo
	cd dataplane && gofmt -w .
	$(UV) run ruff format .
	$(UV) run ruff check --fix .
	$(NER) run ruff format .
	$(NER) run ruff check --fix .

.PHONY: cover
cover: ## Go coverage report, gated at $(COVERAGE_THRESHOLD)%
	COVERAGE_THRESHOLD=$(COVERAGE_THRESHOLD) ./scripts/coverage.sh
