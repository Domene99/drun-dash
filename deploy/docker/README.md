# Local dev stack (Postgres + Redis)

Infrastructure services for local development of the drun-dash control
plane (Go). This stack provides **PostgreSQL 16** for persistent state and
**Redis 7** for budget counters / session cache.

> **Note:** There is no application container yet — only infrastructure
> services. Application services are added in **batch 2**.

## Prerequisites

- Docker Engine with the Compose v2 plugin (`docker compose`, not the
  legacy `docker-compose`).

## Getting started

From `deploy/docker/`:

```bash
# 1. Create your local env file (defaults are fine for local dev)
cp .env.example .env

# 2. Start the stack in the background
docker compose up -d

# 3. Check status / health
docker compose ps

# 4. Tail logs
docker compose logs -f

# 5. Stop the stack (keeps data volumes)
docker compose down

# 6. Stop and wipe data volumes
docker compose down -v
```

Both services declare healthchecks, so `docker compose ps` reports each
container as `healthy` once it is ready to accept connections.

## Connection strings for Go

These match the defaults in `.env.example`. Adjust if you change the
credentials or ports in your `.env`.

```bash
DATABASE_URL=postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable
REDIS_URL=redis://localhost:6379/0
```

- `DATABASE_URL` uses `sslmode=disable` because the local Postgres
  container is not configured for TLS. Do **not** use this setting against
  a production database.
- `REDIS_URL` targets Redis logical database `0`.

## Services

| Service    | Image         | Host port | Volume          |
|------------|---------------|-----------|-----------------|
| `postgres` | `postgres:16` | `5432`    | `postgres_data` |
| `redis`    | `redis:7`     | `6379`    | `redis_data`    |

Named volumes persist data across `docker compose up`/`down`. Remove them
with `docker compose down -v`.

## Configuration

All settings live in `.env` (copied from `.env.example`):

| Variable            | Default    | Description                  |
|---------------------|------------|------------------------------|
| `POSTGRES_USER`     | `drundash` | Postgres role / username     |
| `POSTGRES_PASSWORD` | `drundash` | Postgres password            |
| `POSTGRES_DB`       | `drundash` | Postgres database name       |
| `POSTGRES_PORT`     | `5432`     | Host port mapped to Postgres |
| `REDIS_PORT`        | `6379`     | Host port mapped to Redis    |

`.env` is git-ignored (see `.gitignore`) so local credentials are never
committed.
