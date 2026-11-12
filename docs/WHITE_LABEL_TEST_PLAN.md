# White-Label Test Plan

## How to run
```bash
# PostgreSQL that allows CREATE DATABASE (each test run makes and drops its own DB)
docker run -d --name zieon-dev-postgres -e POSTGRES_USER=zieon -e POSTGRES_PASSWORD=zieon_dev \
  -e POSTGRES_DB=zieon -p 127.0.0.1:55432:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://zieon:zieon_dev@localhost:55432/zieon?sslmode=disable' go test -race -count=1 ./...
```
If `TEST_DATABASE_URL` is unset, the integration tests are **skipped** (not passed). CI must set it.

Frontends:
- Web: `npm run build` (runs a TypeScript type-check).
- Mobile: `flutter analyze lib test && flutter test`.

## Results (2026-11-12, macOS arm64, Go 1.26.3, PostgreSQL 17)
| Suite | Tests | Result |
|---|---|---|
| `internal/httpapi` (end to end over HTTP + real PostgreSQL, `-race`) | 12 | **PASS** (241 s) |
| `internal/mqttgw` (TLS certificate hot reload) | 1 | **PASS** |
| `internal/app` (pricing, state machine, branding validation) | 3 | **PASS** |
| `internal/auth` (JWT tamper, expiry, wrong key; bcrypt) | 2 | **PASS** |
| `internal/secure` (key rotation, tamper detection, webhook signature and replay window) | 3 | **PASS** |
| Migration up → down → up on a fresh DB | manual | **PASS** |
| `zenerlink-web` `tsc -b && vite build` | — | **PASS** |
| `zenerlink-mobile` `flutter analyze lib test` | — | **No issues** |
| `zenerlink-mobile` `flutter test` (API client and branding with mock HTTP, splash) | 4 | **PASS** |
| Visual check: web login themed for `zenerlink` vs `red-real-estate` from the live local API | manual (headless Chrome) | **PASS** |
| Local smoke: sign in, list meters, fund, buy, redeem, activity (curl against `bin/api`) | manual | **PASS** |

## Coverage matrix (requirement → test)
| Requirement | Test |
|---|---|
| Tenant data isolation (API + RLS) | `TestTwoTenantIsolation` |
| Authentication, lockout, refresh rotation and reuse, suspension | `TestRBACAndAuth` |
| Role permissions (customer, staff, admin, platform) | `TestRBACAndAuth`, `TestTwoTenantIsolation` |
| Tenant provisioning and partner onboarding | Every e2e test provisions through `POST /v1/platform/tenants`. Also `TestPartnerSSOAndWebhooks`. |
| Meter assignment (platform → tenant → customer) | Harness `zieonMeter`, `TestTwoTenantIsolation` (cross-tenant assign refused) |
| Device communication (signature, replay, unknown key, no-comms meter) | `TestDeviceTelemetry` |
| Successful purchase → token → delivery | `TestPurchaseDeliveryAndHistory` |
| Duplicate payment webhooks, bad signature, amount mismatch | `TestCardPaymentWebhooks` |
| Concurrent vending (10 parallel, balance 2) | `TestIdempotencyAndConcurrency` |
| Idempotent replay and key reuse with a different body | `TestIdempotencyAndConcurrency` |
| Failed vend → refund; unknown outcome → reconciliation retry | `TestVendFailureRefundAndReconciliation` |
| Delayed payment notification → reconciliation verify | `TestCardPaymentWebhooks` |
| Membership eligibility, cashback %, per-purchase and monthly caps, monthly credit once | `TestBenefitsEngine` |
| Reward abuse: empty pool, duplicate deposit, no cashback on the reward leg | `TestBenefitsEngine` |
| Rewards don't alter tariff or kWh | `TestBenefitsEngine` |
| Spark Share conservation and capability gating | `TestSparkShare` |
| Offline meter detection, low balance, auto top-up, tamper | `TestDeviceTelemetry` |
| Ledger immutability and balance/ledger agreement | `TestLedgerIsAppendOnly` |
| Partner SSO: single-use code, origin check, scopes, revocation, cross-tenant key | `TestPartnerSSOAndWebhooks` |
| MQTT: bad, stale or mismatched credentials refused; forged payload dropped; cross-meter publish disconnects; cross-meter/wildcard subscribe refused; token credit via command, delivered only on `executed` ack; rejected ack returns the token; relay command; offline queue flushed on reconnect; online/offline tracking | `TestMQTTDeviceGateway` |
| Webhook signing, SSRF refusal, retry after a 500 | `TestPartnerSSOAndWebhooks` |
| Mobile retry-after-refresh reuses the Idempotency-Key | `zenerlink-mobile/test/api_test.dart` |

## Not yet tested
- **Load and soak** (target: 50 purchases/s sustained on pilot hardware).
- **Restore drill** from a `pg_dump` on the server.
- **Paystack live sandbox:** needs `PAYSTACK_SECRET_KEY` (test mode) to exercise initialize, verify and webhook end to end.
- **Real meter firmware** speaking the telemetry protocol (blocked on hardware).
- **Browser end-to-end** of the signed-in web flows (only API-level and login-screen visual checks so far). A Playwright suite is recommended.
- **Mobile on device or emulator** against the pilot API (compiled via tests and analyzer only).
