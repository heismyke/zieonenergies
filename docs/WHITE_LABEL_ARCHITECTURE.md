# White-Label Architecture

## System overview

```
 Partner apps (PCE app, Red dashboard)          Zieon first-party apps
 ┌──────────────────────────────┐               ┌──────────────────────────┐
 │ Partner backend ──API key──┐ │               │ zenerlink-web  (React)   │
 │ Partner UI ── iframe/webview│ │               │ zenerlink-mobile (Flutter)│
 └─────────────────────────────┼─┘               └────────────┬─────────────┘
                               │  HTTPS (Caddy TLS)            │ HTTPS + JWT
                               ▼                               ▼
                 ┌───────────────────────────────────────────────────┐
                 │  zieon-api  (Go, single binary, /v1 REST)          │
                 │  httpapi: auth, CORS, rate limit, validation       │
                 │  app:     tenants · identity · meters · vending    │
                 │           benefits · admin · workers (outbox)      │
                 │  vending.Provider ─┐   payments.Provider ─┐        │
                 └────────────────────┼──────────────────────┼────────┘
                                      ▼                      ▼
                       Sandbox | (live STS/DisCo     Sandbox | Paystack
                        partner — BLOCKED)
                 ┌───────────────────────────────────────────────────┐
                 │ PostgreSQL 17 · RLS per tenant · append-only ledger │
                 └───────────────────────────────────────────────────┘
                                      ▲
           Zieon meters ──HMAC-signed telemetry── POST /v1/devices/telemetry
```

## Key decisions

| Decision | Choice | Why |
|---|---|---|
| Service shape | **Modular monolith** (one Go binary, with background workers in-process) | Pilot scale is two partners and a few hundred meters. Microservices would add network failure modes to money flows for no measurable gain. Workers use `FOR UPDATE SKIP LOCKED`, so more instances can be added safely. |
| Language/DB | Go 1.26 stdlib `net/http` + `pgx/v5` + PostgreSQL 17 | Matches the prompt's expected stack and the other services on the host. Only 3 direct dependencies: `pgx`, `x/crypto` (bcrypt) and `google/uuid`. |
| Tenancy | Shared schema, `tenant_id` on every row, **PostgreSQL RLS** as a second line of defence | Cheapest to operate at this scale. RLS catches a forgotten `WHERE tenant_id`. |
| Money | Integer kobo, double-entry postings, append-only `ledger_entries` | No float rounding. Exactly-once postings come from a unique (account, kind, reference). |
| Energy | Integer watt-hours, append-only `energy_entries` | Shares and deliveries are conserved (asserted in tests). |
| Partner UX | **Hosted module + REST + webhooks; no SDK** | Partners embed the existing web app (`?embed=1&tenant=…#code=…`) or call REST from their backend. A native SDK would need maintaining in 3+ languages and adds nothing the REST contract lacks. Revisit if a partner needs a native in-app experience without a webview. |
| Vending | `vending.Provider` interface with **Sandbox** only | We have no STS keys or DisCo vending agreement. The interface is the seam where an authorised partner plugs in. |
| Payments | `payments.Provider`: Sandbox and Paystack | Paystack is a CBN-licensed PSP. Using it avoids handling card data (PCI scope stays with the PSP). |

## Request lifecycle
1. `recoverer → requestMeta (X-Request-Id, client IP) → securityHeaders → cors → mux`.
2. The handler authenticates the caller:
   - `user()`: verifies the JWT, re-checks that the user and tenant are still active, applies the per-principal rate limit, and rejects a mismatched `X-Tenant`.
   - `apiKey(scope)`: verifies the API key and its scope.
   - `AuthenticateDevice`: verifies the HMAC signature and nonce.
3. The handler decodes strict JSON and calls `app.Service`.
4. The service opens `db.Store.Tenant(ctx, tenantID, fn)`:
   - `BEGIN; SET LOCAL ROLE zieon_app; set_config('app.tenant_id', …, true)`
   - every statement is then subject to RLS.
5. State changes, ledger postings, audit rows and webhook outbox rows commit atomically.

## Database
Defined in `internal/db/migrations/0001_init.up.sql`. Rollback is `0001_init.down.sql`, which is development-only; production rollback is restore-from-backup. The table groups are:

- **Tenancy:** `tenants`, `tenant_branding`
- **Identity:** `users`, `refresh_tokens`, `api_keys`, `embed_codes`
- **Assets:** `properties`, `tariffs`, `meters`, `meter_links`, `meter_transfers`, `meter_readings`, `meter_alerts`, `device_nonces`
- **Money:** `balances`, `ledger_entries` (immutable), `payments`, `webhook_events`
- **Vending:** `vend_transactions`, `energy_tokens` (AES-GCM encrypted plus SHA-256 lookup hash), `energy_entries` (immutable), `idempotency_keys`, `share_recipients`, `auto_topup_jobs`
- **Benefits:** `membership_tiers`, `memberships`, `reward_grants`
- **Engagement:** `notifications`, `support_tickets`, `support_messages`, `subscription_plans`
- **Partner and ops:** `webhook_endpoints` (secret encrypted), `webhook_deliveries` (outbox), `audit_logs` (immutable)

New migrations are added as `000N_name.up.sql` / `.down.sql`. They are applied in order on boot, under an advisory lock.

## Deployment topology (pilot)
- Host `198.199.65.142` (shared, Ubuntu 24.04), with Compose project `zieon-prod` in `~/apps/zieon`.
- `api` joins `hixmastudio-proxy_default` with the alias `zieon-api`. Caddy serves `https://zieon-api.198.199.65.142.sslip.io`.
- `db` is Postgres 17 with a named volume and no published port.
- Memory limits are api 256 MiB and db 384 MiB (the host has about 2 GiB shared with 10+ other services).

## Frontend integration
- **Web** (`zenerlink-web`):
  - `src/lib/api.ts` is the typed client. It does refresh-token rotation, adds idempotency keys, and selects the tenant from `?tenant=` or `VITE_TENANT`.
  - `src/lib/theme.ts` maps tenant theme tokens to CSS variables. `tailwind.config.ts` colours read `rgb(var(--zener-*))`.
- **Mobile** (`zenerlink-mobile`):
  - `ApiClient` (http + secure storage) and `ZieonApi` (typed facade).
  - `Brand` is the runtime palette, loaded before the first frame.
  - Build per partner with `--dart-define=TENANT=pce-energy --dart-define=API_URL=…`.
