# Security and Threat Model

This document follows the OWASP API Security Top 10 (2023) and STRIDE.

## Assets
- Customer funds: wallet and reward balances, and payments.
- Electricity tokens: bearer value.
- Partner API keys and webhook secrets.
- Device secrets.
- PII: email, phone, address, meter number.
- Audit integrity.

## Controls by OWASP API risk
| OWASP API risk | Threat | Control | Verified by |
|---|---|---|---|
| API1 Broken object-level authz | Read or act on another customer's or tenant's meter, purchase or token | Every customer query joins `meter_links.user_id`, plus `tenant_id` predicates and **RLS**. Foreign ids return 404. Tokens are bound to `meter_id`. | `TestTwoTenantIsolation` |
| API2 Broken authentication | Credential stuffing, token theft | bcrypt cost 12, constant-time dummy check for unknown emails, lockout after 5 failures (15 min), IP rate limit on auth. Access tokens last 15 min. Refresh tokens are rotated, stored hashed, and reuse revokes the whole family. | `TestRBACAndAuth` |
| API3 Broken property-level authz | Mass assignment (e.g. `role` in sign-up) | `DisallowUnknownFields` on every body. Role is never client-settable. Tokens are revealed only to the purchasing customer and never to partners or staff. | `TestRBACAndAuth` (unknown field → 400) |
| API4 Unrestricted resource consumption | Floods, large bodies | 1 MiB body cap, server timeouts, per-IP/user/key/device token buckets, list limits capped. | — |
| API5 Broken function-level authz | Customer or staff calling admin or platform endpoints | Role guards per handler. Platform role is valid only inside the platform tenant. Tariffs and vending mode are platform-only. | `TestRBACAndAuth` |
| API6 Sensitive business flows | Double credit, replayed purchases, reward farming | Idempotency keys, row locks, state machine, unique ledger postings, unique reward `source_ref`, monthly cashback caps, cashback earned only on the paid leg, pool never overdrawn. | Idempotency/Concurrency, Benefits, CardPayment tests |
| API7 SSRF | Webhook URL pointing at internal services | URL must be https and a public host. The **dialer refuses private, loopback and link-local IPs at connect time** (defeats DNS rebinding). Redirects are not followed. | `TestPartnerSSOAndWebhooks` |
| API8 Misconfiguration | Weak headers, open CORS | `nosniff`, `X-Frame-Options: DENY`, `frame-ancestors 'none'`, HSTS, `no-store`. CORS is an allowlist (env plus per-tenant origins). Production refuses to start without a live payment key and disables sandbox providers. | — |
| API9 Inventory | Forgotten endpoints | Single route table (`httpapi/routes.go`) and a `/v1` prefix. | — |
| API10 Unsafe consumption of APIs | Trusting provider webhooks | Paystack HMAC-SHA512 **and** re-verification via the Paystack API, amount match required, event de-duplication. | `TestCardPaymentWebhooks` |

## STRIDE highlights
- **Spoofing.**
  - Devices: HMAC with per-device secrets, a ±5 min timestamp and single-use nonces (`TestDeviceTelemetry` covers replay and a bad key).
  - Webhooks: signed both ways.
- **Tampering.**
  - JWT HS256 with a `kid`. Algorithm is pinned and `iss`/`aud` are checked.
  - `ledger_entries`, `energy_entries` and `audit_logs` are append-only: a trigger blocks even the table owner, and UPDATE/DELETE are revoked from `zieon_app` (`TestLedgerIsAppendOnly`).
- **Repudiation.** Privileged actions write `audit_logs` rows with actor, IP and request id. Platform-admin views of tenant data are audited.
- **Information disclosure.**
  - Secrets are hashed (API keys, refresh tokens, embed codes) or AES-256-GCM encrypted (tokens, device and webhook secrets).
  - Secrets are never logged. Request logs record only method, path, status, latency, ip and request id.
  - Internal errors return a generic message plus a request id.
- **Denial of service.** Rate limits and timeouts, plus container memory limits.
- **Elevation of privilege.** `SET LOCAL ROLE zieon_app` (NOSUPERUSER, NOBYPASSRLS) for all application SQL.

## Key management and rotation
| Secret | Storage | Rotation |
|---|---|---|
| `JWT_SIGNING_KEY` | Host env (mode 600) | Set a new key and a new `JWT_KEY_ID`, then restart. All access tokens expire within 15 minutes, and refresh tokens keep working (they are opaque, not JWTs). Dual-key verification is **not yet implemented**: rotating forces a re-login after at most 15 minutes. |
| `DATA_ENCRYPTION_KEYS` | Host env, `id:base64` list | Add `k2:…`, set `DATA_ENCRYPTION_KEY_ID=k2`, restart. Old rows still decrypt via `k1`, and new writes use `k2`. Re-encryption is a planned job. Retire `k1` only after re-encryption. |
| API keys and webhook secrets | Hashed / encrypted in DB | Partner creates a new one, switches over, then revokes the old one. |
| Device secrets | Encrypted in DB, copy in firmware | Needs a firmware re-provisioning flow. **Not built (blocked on hardware).** |
| Postgres password | Host env | Change in Postgres and `.env.production`, then restart. |

**Gap:** there is no HSM or KMS. For production, move keys to a managed KMS or Vault and inject them at runtime.

## Data protection
- **PII stored:** name, email, phone, meter number, address. No card data, because the PSP-hosted checkout keeps it outside our PCI scope.
- **Retention:** nonces 1 day, idempotency keys 7 days, embed codes 1 day, expired refresh tokens 7 days, raw provider webhooks 90 days.
- **Ledgers and audit logs** are retained indefinitely for financial records. The legal retention period must be confirmed.
- **Gap:** there is no data-subject erasure or export endpoint yet (NDPA rights). The plan is to pseudonymise user PII while keeping ledger rows.

## Residual risks
1. In-process rate limiting is per instance.
2. Nothing alerts on security events yet (failed-login bursts, signature failures). They are only logged and audited.
3. A shared host means other workloads on the machine are in the blast radius. Production should run on dedicated infrastructure.
4. Sandbox mode is controlled by `APP_ENV`. A mis-set environment on a production host would enable simulated funding. Mitigations:
   - the deploy checklist
   - the `"sandbox": true` flag on every response
   - the UI badge
