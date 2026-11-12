# zieonenergies

White-label smart-metering and electricity-vending platform (Metering-as-a-Service) for Zieon Energies.
Go 1.26 + PostgreSQL 17. It serves the Zenerlink web app (`zenerlink-web`), the Zenerlink mobile app (`zenerlink-mobile`) and partner tenants such as PCE Energy and Red Real Estate.

> **Status: development ready, running as a sandbox pilot.** Electricity tokens come from a **sandbox** vending provider. They are flagged `sandbox: true`, start with `99`, and are not valid on real meters. Live vending needs an authorised STS/DisCo vending partner. See `docs/PRODUCTION_LAUNCH_CHECKLIST.md`.

## Quick start
```bash
docker compose up -d db                     # Postgres on 127.0.0.1:55432
cp .env.example .env                        # fill JWT_SIGNING_KEY and DATA_ENCRYPTION_KEYS
set -a; . ./.env; set +a
go run ./cmd/api migrate
go run ./cmd/api seed-pilot                 # prints sandbox tenants + credentials once
go run ./cmd/api                            # serves on :8080
```
Tests (integration tests need a Postgres that allows `CREATE DATABASE`):
```bash
TEST_DATABASE_URL='postgres://zieon:zieon_dev@localhost:55432/zieon?sslmode=disable' go test -race ./...
```

## Layout
| Path | Purpose |
|---|---|
| `cmd/api` | Entrypoint. Commands: `serve` (default), `migrate`, `migrate-down` (non-production only), `seed-pilot` |
| `internal/app` | Business rules: tenants, identity, meters, vending, benefits, admin, workers |
| `internal/httpapi` | `/v1` REST handlers, middleware, and end-to-end tests |
| `internal/db` | Pool, embedded migrations, `Tenant()`/`System()` RLS transaction helpers |
| `internal/auth`, `internal/secure` | JWT and bcrypt; AES-GCM keyring, HMAC signatures |
| `internal/vending`, `internal/payments` | Provider interfaces (sandbox, Paystack) |
| `deploy/` | Production env template and backup script |
| `docs/` | Audit, architecture, tenancy, vending flow, partner guide, security, regulatory, test plan, launch checklist |
