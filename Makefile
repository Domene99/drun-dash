.PHONY: test test-integration lint-openapi docker-up docker-down docker-ps validate

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
