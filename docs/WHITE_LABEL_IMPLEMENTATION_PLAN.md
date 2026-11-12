# White-Label Implementation Plan

## Delivered in this iteration (Phases 1–5)
| Phase | Output |
|---|---|
| 1. Audit | `WHITE_LABEL_READINESS_AUDIT.md`. Finding: there was no backend; both apps were mock-only. |
| 2. Architecture | `WHITE_LABEL_ARCHITECTURE.md`, `MULTI_TENANCY_DESIGN.md`, `SECURITY_AND_THREAT_MODEL.md` |
| 3. Implementation | Go API: 95 routes. Schema: 34 tables (29 tenant tables under RLS). Workers. Web and mobile wired to the API with runtime theming. |
| 4. Verification | 19 Go tests (race detector), 4 Flutter tests, analyzer and type-checks. See `WHITE_LABEL_TEST_PLAN.md`. |
| 5. Pilot prep | `api seed-pilot` provisions Zenerlink, PCE Energy and Red Real Estate (sandbox meters, tiers, a ₦500k sandbox pool). |

## Next increments (ordered by risk reduction)
| # | Work | Depends on | Size |
|---|---|---|---|
| 1 | **Vending partner integration**: implement `vending.Provider` for the chosen STS/KMC vendor or DisCo aggregator, plus contract tests against their sandbox. | Commercial agreement | M |
| 2 | **Wallet licensing decision**: either move to a licensed partner's virtual accounts or disable `wallet` and use per-purchase checkout. | Legal opinion | M |
| 3 | **Paystack test-mode end to end**: also add refund-to-card via the Paystack Refund API (cash-out of `refunded` card legs). | Test keys | S |
| 4 | **Observability**: Prometheus `/metrics` (vend outcomes, webhook lag, pending payments), alerts (needs_review > 0, offline meters, 5xx rate), error tracking. | — | S |
| 5 | **Backups**: off-host copy (S3/Spaces), weekly restore drill, PITR via WAL archiving. | Bucket | S |
| 6 | **Firmware telemetry client**: provisioning of the device secret at manufacture, `accepted_tokens` reporting. Then a downlink channel (MQTT) for remote credit and relay. | Hardware team | L |
| 7 | **Email/SMS notifications**: tenant-branded templates (Termii / SES). | Provider | S |
| 8 | **Custom domains**: per-tenant hosted module with CSP `frame-ancestors`. Caddy on-demand TLS for partner domains. | DNS from partners | M |
| 9 | **Tenant admin UI**: today admins use the API. Build an admin console over `/v1/admin/*`. | — | M |
| 10 | **Shared rate limiting** (Redis) and a horizontal-scale test. | — | S |
| 11 | **NDPA tooling**: data export and erasure (pseudonymise PII, keep ledgers), consent records. | DPO | S |
| 12 | **Exchange tab**: the mobile P2P "Energy Exchange" chat remains a local prototype (flag `energy_exchange` off). Either remove it or build it on `/v1/shares`. | Product | S |
| 13 | **Subscriptions checkout**: plans are served by `/v1/plans`. Billing needs payment recurrence. | Payments | M |
