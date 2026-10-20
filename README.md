# stripe-clone

A small payments API built to learn backend and system design.

## Run

```sh
docker compose up -d --wait                                            # Postgres 17 on :5433
export DATABASE_URL='postgres://stripe:stripe@localhost:5433/stripe?sslmode=disable'
go run ./cmd/migrate up                                                # also: down, status
go run ./cmd/api                                                       # listens on :8080 (override with PORT)
curl localhost:8080/healthz                                            # 200 when the DB is up, 503 when down
```

## Test

```sh
go test ./...   # the migrations test starts its own Postgres via testcontainers, so Docker must be running
```
