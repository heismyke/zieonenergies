# White-Label Readiness Audit

**Date:** 2026-11-12 · **Scope:** `zenerlink-web` (React/Vite), `zenerlink-mobile` (Flutter), and this repository (`zieonenergies`, new Go backend).

## 1. Audit findings: what existed before this work

The brief assumed an existing Go/PostgreSQL smart-metering backend. **No backend existed.** I inspected the entire `/Users/myke/zieon` workspace:

| Item | Finding | Evidence |
|---|---|---|
| Backend service | **None.** No Go, Node or other server code. No database, migrations, Docker files or CI for an API. | Workspace listing. The only Docker files are in `website/`, a marketing site. |
| Web app | React 18 + Vite + Tailwind. A single `App.tsx` (978 lines) holds all state in `useState`. | `zenerlink-web/src/App.tsx`, `src/data/mockData.ts` |
| Mobile app | Flutter 3.41. All state lives in `_MainNavigationState` (5,910 lines). `api_constants.dart` points at a non-existent `https://api.zenerlink.app/v1`. `dio_client.dart` is an empty class. | `zenerlink-mobile/lib/features/navigation/main_navigation.dart` |
| Authentication | **Fake.** Both apps accept any input. Neither checks the password. Both fall back to a hardcoded email (`myke@zenerlink.app`). | `App.tsx` `AuthScreen`; `login_page.dart:43-52` |
| Electricity tokens | **Generated on the client** with `Math.random()` / `Random()`. Nothing about them is real STS. | `lib/format.ts generateToken`; `main_navigation.dart:5873` |
| Wallet | A client-side number that starts at ₦25,000. "I have made this transfer" credits it with no verification. Every attempt produces a random 10-digit "account number". | `main_navigation.dart:66, 4494, 4606` |
| Tariff | ₦200/kWh, hardcoded in 4 places. | `App.tsx`, `app_constants.dart:5`, `main_navigation.dart:220, 2338` |
| Meter protocols | **None.** Nothing in the code covers GSM, MQTT, Wi-Fi, STS or DLMS. No hardware or firmware repository is present. | — |
| Payments / webhooks | None. | — |
| Multi-tenancy / branding | None. The Zenerlink palette is hardcoded: 94 `Color(0xFFC8823A)` literals in mobile, and Tailwind constants in web. | `tailwind.config.ts`, `theme.dart` |
| Logging, monitoring, backups | None. | — |
| Tests | One splash-screen widget test (mobile). No web tests. | `zenerlink-mobile/test/widget_test.dart` |
| Existing bugs | The mobile support chat crashes because `.add` is called on a `const` list. The Exchange chat's kWh regex is double-escaped and never matches. | `main_navigation.dart:4349, 3034` |

**Conclusion:** the frontends are high-fidelity prototypes that run on mock data. The backend in this repository is new work, built to the contracts the two apps imply. "Preserve existing functionality" therefore meant keeping the frontend UX and flows intact and wiring them to real data.

## 2. Requirement classification (after this work)

Legend:
- **IMPLEMENTED**: built and covered by automated tests run against PostgreSQL 17.
- **PARTIAL**: works but with known gaps.
- **MISSING**: not built.
- **BLOCKED**: needs hardware, a partner or a credential we do not have.
- **EXT-APPROVAL**: requires external regulatory or commercial approval.

### Multi-tenancy
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Tenant provisioning and unique ids | IMPLEMENTED | `app.ProvisionTenant`, `POST /v1/platform/tenants`. Test: `TestTwoTenantIsolation`. | — |
| Tenant settings and feature flags | IMPLEMENTED | `tenants.features`, `KnownFeatures`. Unknown flags are rejected. | — |
| Users, meters, properties and transactions tied to a tenant | IMPLEMENTED | Every tenant table has a `tenant_id NOT NULL` column. Unassigned meter inventory is NULL by design. | — |
| Isolation in middleware, services and SQL | IMPLEMENTED | Explicit predicates **plus** PostgreSQL RLS (`FORCE ROW LEVEL SECURITY`, `SET LOCAL ROLE zieon_app`). The test proves RLS hides other tenants' rows even with no `WHERE` clause and refuses cross-tenant inserts. | — |
| Tenant context from credentials only | IMPLEMENTED | `tid` comes from the signed JWT. A conflicting `X-Tenant` header is rejected with 403. | — |
| Tenant-scoped RBAC, platform vs tenant admins | IMPLEMENTED | Roles are `platform_admin`, `tenant_admin`, `tenant_staff` and `customer`. Test: `TestRBACAndAuth`. | — |
| Automated isolation tests | IMPLEMENTED | `TestTwoTenantIsolation` (PCE vs Red end to end). | — |

### Branding
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Name, logo, colours, support info, currency, email branding | IMPLEMENTED | `tenant_branding` table. Validated (`#RRGGBB` colours, `https` URLs only). | — |
| Custom domain or subdomain | PARTIAL | Stored, unique, and resolved from `Host`. **DNS, TLS issuance and CDN routing for partner domains are not automated.** | Med |
| Frontends consume branding | IMPLEMENTED | Web uses CSS variables; mobile uses the `Brand` runtime palette. Screenshots were verified for Zenerlink and Red Energy. | — |
| Notification email branding | PARTIAL | Branding fields exist, but **no email/SMS sender is integrated**. Notifications are in-app only. | Med |

### Partner integration
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| SSO, with short-lived single-use codes | IMPLEMENTED | `POST /v1/partner/sessions` mints a code valid for 2 minutes. `POST /v1/embed/exchange` turns it into a ≤15-minute token with no refresh. Test: `TestPartnerSSOAndWebhooks`. | — |
| Approved origins and restricted embedding | PARTIAL | CORS allowlist per tenant and `return_origin` check: done. A `frame-ancestors` CSP **must be set by whatever host serves the web module** (static hosting today). | Med |
| Scoped API keys | IMPLEMENTED | 6 scopes, hashed at rest, shown once, revocable, expirable. | — |
| API versioning | IMPLEMENTED | `/v1` prefix. Webhook payloads carry `api_version`. | — |
| Rate limiting | PARTIAL | In-process token buckets per IP, user, API key and device. **Per instance only.** Needs Redis or an edge limiter before scaling out. | Med |
| Validation and idempotency | IMPLEMENTED | Strict JSON with unknown fields rejected, 1 MiB body cap. `Idempotency-Key` is required on money-moving POSTs. | — |
| Signed outbound webhooks with retries | IMPLEMENTED | `Zieon-Signature` HMAC-SHA256 with a timestamp. Exponential backoff, 8 attempts, transactional outbox, SSRF-guarded dialer. | — |
| SDK | Not built (by decision) | REST + hosted module + webhooks is enough. See the architecture doc. | — |

### Meters
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Registration, unique serials, tenant assignment | IMPLEMENTED | `RegisterMeter` and `AssignMeterToTenant`. Zieon serials are globally unique. External meters are unique per tenant. | — |
| Customer and property links, activation and deactivation | IMPLEMENTED | `meter_links`, `properties`, `SetMeterStatus`. | — |
| Ownership transfer with approval and audit | IMPLEMENTED | `meter_transfers`. Only one pending transfer per meter. Admin decision is audited. | — |
| Device authentication | IMPLEMENTED (server) / BLOCKED (firmware) | Signed HTTP telemetry **and an MQTT gateway**. The gateway uses a time-bound HMAC password plus signed envelopes in both directions, and per-device topic ACLs (`docs/MQTT_DEVICE_PROTOCOL.md`, `TestMQTTDeviceGateway`). **No firmware implements it yet.** | High |
| Connectivity, readings, consumption, firmware version | IMPLEMENTED (server side) | Offline detection, register-based consumption, firmware field. Data appears only once devices report. | — |
| Tamper and abnormal alerts | IMPLEMENTED (server side) | `meter_alerts` plus webhook. Depends on the meter reporting a tamper condition. | — |
| Remote credit or relay commands to meters | IMPLEMENTED (server) / BLOCKED (firmware) | `device_commands` queue delivered over MQTT. Covers `credit_token`, `relay`, `sync` and `ping`, with at-least-once delivery and an idempotent command id. A token counts as delivered only on the device's signed `executed` ack. Needs firmware. | High |

### Vending
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Quote (tariff, fee, VAT) | IMPLEMENTED | `computeQuote`. Unit-tested. Rewards never alter it. | — |
| State machine, atomicity, idempotency, duplicate-credit prevention | IMPLEMENTED | `transitions`, row locks, unique ledger postings. Tests cover concurrency, webhook duplicates and replays. | — |
| Failed-vend recovery, refunds, reconciliation | IMPLEMENTED | Rejected vends are refunded automatically. Unknown outcomes go to `needs_review` after 30 minutes. Operators can resolve them. | — |
| Immutable ledger | IMPLEMENTED | Triggers and REVOKE. Test: `TestLedgerIsAppendOnly`. | — |
| Real STS token generation / DisCo vending | **BLOCKED + EXT-APPROVAL** | Only a **sandbox** provider exists. Its tokens start with `99` and are flagged `sandbox: true`. Production refuses sandbox vending. | **Critical** |
| Payment collection | PARTIAL | Paystack initialise, verify and webhook HMAC are written but **not tested against live Paystack**. Sandbox provider for pilots. | High |
| Notifications | PARTIAL | In-app plus partner webhooks. No SMS or email. | Med |

### Benefits
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Configurable tiers, % cashback, caps, fixed rewards, monthly credits, expiry | IMPLEMENTED | `membership_tiers`, `reward_grants`. Test: `TestBenefitsEngine`. | — |
| Partner-funded pool, never overdrawn | IMPLEMENTED | Pool balance check plus a non-negative balance constraint. | — |
| Duplicate-claim and abuse prevention | IMPLEMENTED | Unique `(user, kind, source_ref)`. Monthly cap. Cashback is earned only on the paid leg. | — |
| Rewards kept separate from tariff | IMPLEMENTED | Rewards are a separate funding leg. Energy and rate stay unchanged (asserted in tests). | — |

### Portfolio
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Multiple properties and meters, balances, history, low-balance alerts | IMPLEMENTED | `GET /v1/properties`, low-balance notifications that fire once per dip. | — |
| Automatic top-ups | IMPLEMENTED (wallet) | Telemetry queues a job and the worker purchases from the wallet. Needs telemetry-capable meters. No card-on-file auto-charge. | Med |

### Security, operations and compliance
| Requirement | Status | Evidence / gap | Risk |
|---|---|---|---|
| Password hashing, lockout, refresh rotation and reuse detection | IMPLEMENTED | bcrypt cost 12, 5-attempt lockout, refresh-family revocation. | — |
| Encryption at rest of secrets and tokens | IMPLEMENTED | AES-256-GCM keyring with key ids (rotation tested). | — |
| Encryption in transit | IMPLEMENTED (edge) | Caddy terminates TLS. The API to Postgres hop is inside the Docker network without TLS. | Low |
| Secrets management | PARTIAL | Host `.env.production` with mode 600. **No vault or KMS.** | Med |
| Audit logs | IMPLEMENTED | Append-only. Privileged actions and platform access inside a tenant are recorded. | — |
| Structured logs and request ids | IMPLEMENTED | JSON `slog`, `X-Request-Id`. | — |
| Metrics, tracing, alerting, error monitoring | MISSING | Only `/healthz` and `/readyz`. | High |
| Backups | PARTIAL | `deploy/backup.sh` (pg_dump). **No off-host copy, and restore has not been drilled on the server.** | High |
| Data retention and deletion | PARTIAL | Cleanup of nonces, keys and codes. **No customer data-erasure workflow.** | Med |
| NERC, NEMSA, SON, NDPC, CBN | EXT-APPROVAL | See `NIGERIA_REGULATORY_READINESS.md`. **No approvals are evidenced.** | Critical |

## 3. Highest-risk gaps (summary)
1. **No authorised vending route.** We cannot issue real electricity credit. We need an STS key-management and vending partner, or DisCo vendor agreements.
2. **Customer wallet may be a regulated stored-value product.** Holding naira balances needs a CBN-licensed partner (legal verification required).
3. **Hardware integration is unproven.** Device protocol is specified, but no firmware implements it.
4. **Observability and backup drills are missing.**
