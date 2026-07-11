.PHONY: test test-integration lint-openapi docker-up docker-down docker-ps validate \
	build run-control-plane run-proxy tools migrate-up migrate-down seed dev

# Dev database URL. Override on the command line, e.g.
#   make migrate-up DATABASE_URL=postgres://user:pass@host:5432/db?sslmode=disable
DATABASE_URL ?= postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable

# Run all Go unit tests (integration tests excluded by default).
test:
	go test ./... -count=1

# Run Go tests including drun-mcp integration tests (requires drun-mcp on PATH).
test-integration:
	go test ./... -count=1 -tags=integration

# Lint the OpenAPI contract with Spectral (ad-hoc via npx; no Node app in repo).
lint-openapi:
	npx --yes @stoplight/spectral-cli lint openapi/v1.yaml

# Start local Postgres + Redis dev stack.
docker-up:
	docker compose -f deploy/docker/docker-compose.yml --env-file deploy/docker/.env.example up -d

docker-down:
	docker compose -f deploy/docker/docker-compose.yml down

docker-ps:
	docker compose -f deploy/docker/docker-compose.yml ps

# Full validation suite for batch 1 artifacts.
validate: test lint-openapi docker-up
	@echo "All batch 1 validations passed."

# ---------------------------------------------------------------------------
# Developer build / run / migration / seed targets (batch 3)
# ---------------------------------------------------------------------------

# Compile every cmd/ binary into ./bin.
build:
	go build -o bin/ ./cmd/...

# Run the control-plane service from source.
run-control-plane:
	go run ./cmd/control-plane

# Run the LLM proxy service from source.
run-proxy:
	go run ./cmd/llm-proxy

# Install dev CLIs (golang-migrate with the postgres driver).
tools:
	go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# Apply all pending migrations against DATABASE_URL (run `make docker-up` first).
migrate-up:
	@command -v migrate >/dev/null 2>&1 || { echo "migrate not found on PATH; run 'make tools' first."; exit 1; }
	migrate -path migrations -database "$(DATABASE_URL)" up

# Roll back the most recent migration against DATABASE_URL.
migrate-down:
	@command -v migrate >/dev/null 2>&1 || { echo "migrate not found on PATH; run 'make tools' first."; exit 1; }
	migrate -path migrations -database "$(DATABASE_URL)" down 1

# Load seed data into DATABASE_URL (seed SQL lands in a sibling batch 3 issue).
seed:
	@command -v psql >/dev/null 2>&1 || { echo "psql not found on PATH; install the PostgreSQL client to seed."; exit 1; }
	@test -f deploy/seed/seed.sql || { echo "deploy/seed/seed.sql not found yet; nothing to seed."; exit 0; }
	psql "$(DATABASE_URL)" -f deploy/seed/seed.sql

# One-command dev loop: start docker, wait for Postgres health, then migrate.
dev: docker-up
	@echo "Waiting for Postgres to become healthy..."
	@for i in $$(seq 1 30); do \
		status=$$(docker inspect -f '{{.State.Health.Status}}' drundash-postgres 2>/dev/null || echo "starting"); \
		if [ "$$status" = "healthy" ]; then echo "Postgres is healthy."; break; fi; \
		if [ "$$i" = "30" ]; then echo "Postgres did not become healthy in time."; exit 1; fi; \
		sleep 2; \
	done
	@$(MAKE) migrate-up
