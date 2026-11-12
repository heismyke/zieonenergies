# Production Launch Checklist

**Verdict at time of writing: DEVELOPMENT READY. Deployed to a pilot environment in sandbox mode.** It is **not** pilot-ready for real customers, and not production-ready. Evidence and blockers are below.

## Completed (verified)
- [x] Multi-tenant schema with RLS and isolation tests (PCE vs Red)
- [x] AuthN/AuthZ: roles, lockout, refresh rotation, partner SSO, scoped API keys
- [x] Vending state machine, idempotency, concurrency safety, refunds, reconciliation, immutable ledger
- [x] Benefits engine with caps and a partner-funded pool
- [x] Signed device telemetry protocol (server side), low-balance, offline and tamper handling
- [x] Signed outbound webhooks with retries and SSRF protection
- [x] Web and mobile apps wired to the API, with runtime white-label theming
- [x] Containerised and deployed behind TLS on the pilot host (sandbox)

## Blockers for a **real-customer pilot** (PILOT READY)
- [ ] Authorised vending route (STS vendor/KMC or DisCo agreement) implemented and contract-tested
- [ ] Legal position on the wallet (CBN) and the vending/collection role. Otherwise disable `wallet`.
- [ ] Paystack (or another licensed PSP) live-tested in test mode, then live keys in `.env.production`
- [ ] NEMSA certification for any Zieon meter installed. SON MANCAP/SONCAP for the meter model.
- [ ] NDPA: privacy notice in both apps, DPAs with PCE and Red, DPIA, breach runbook
- [ ] Off-host backups plus one successful restore drill
- [ ] Alerts on `needs_review`, 5xx rate, webhook failures and DB health
- [ ] `APP_ENV=production` on the live stack, confirming sandbox providers are disabled (check `GET /v1/branding` shows `"sandbox": false`)

## Additional for PRODUCTION READY
- [ ] Dedicated infrastructure (not a shared host), managed Postgres with PITR, two or more API instances, shared rate limiter
- [ ] Secrets in a KMS or Vault, with a dual-key JWT verification rotation procedure
- [ ] Load test at 10× expected peak, and a chaos test of vending provider timeouts
- [ ] Firmware integrated and field-tested: telemetry, token acceptance, downlink
- [ ] Penetration test by a third party
- [ ] SLOs agreed with partners and on-call rota staffed

## Service-level objectives (proposed)
| SLI | Objective |
|---|---|
| API availability (non-5xx on `/v1/*`) | 99.5% monthly (pilot), 99.9% (production) |
| Purchase latency (wallet, sandbox or provider p95) | < 3 s |
| Vend completion (`vended` or `refunded` within 5 min of paid) | 99% |
| Stuck in `needs_review` | 0 older than 4 business hours |
| Webhook delivery (first attempt success) | 99% within 1 min |
| Telemetry ingest p95 | < 500 ms |
| RPO / RTO | 24 h / 4 h today (nightly dump). Target 5 min / 1 h with PITR. |

## Deployment (pilot host)
```bash
# on the server, in ~/apps/zieon (git checkout of this repo)
cp deploy/env.production.example .env.production && chmod 600 .env.production   # fill in secrets
docker compose --env-file .env.production -f docker-compose.prod.yml up -d --build
docker compose --env-file .env.production -f docker-compose.prod.yml exec api /app/api seed-pilot   # non-production only
curl https://zieon-api.198.199.65.142.sslip.io/healthz
```
Caddy (`~/apps/reverse-proxy/Caddyfile`):
```
zieon-api.198.199.65.142.sslip.io {
    reverse_proxy zieon-api:8080
}
```

## Backup and restore
- **Backup** (nightly cron): `deploy/backup.sh` writes `backups/zieon-<ts>.dump` (pg_dump custom format, mode 600). **Copy off-host (not yet configured).**
- **Restore:**
  ```bash
  docker compose --env-file .env.production -f docker-compose.prod.yml stop api
  docker compose --env-file .env.production -f docker-compose.prod.yml exec -T db pg_restore -U zieon -d zieon --clean --if-exists < backups/zieon-<ts>.dump
  docker compose --env-file .env.production -f docker-compose.prod.yml start api
  ```
- **Schema rollback in production = restore.** `migrate-down` is refused when `APP_ENV=production`.

## Incident response (summary)
1. **Detect:** health check, user report, or alert (once built).
2. **Triage:** check `docker compose logs api | grep <request_id>`, `GET /readyz`, and Postgres health.
3. **Vending incidents:**
   - Set the affected tenant to suspended only if funds are at risk.
   - Workers keep reconciling.
   - Resolve `needs_review` via `/v1/platform/.../resolve` **after** confirming with the vending partner's records.
4. **Suspected breach:**
   - Rotate `JWT_SIGNING_KEY` (forces re-login) and revoke API keys.
   - Rotate the data key (add a new key id).
   - Preserve logs and audit rows.
   - Notify NDPC within 72 hours where personal data is affected, and notify partners per the DPA.
5. **Post-incident:** write a review covering timeline, root cause and follow-ups.

## Disaster recovery
- **Host loss:** provision a new host, restore the latest off-host dump, redeploy from git, and repoint DNS (sslip.io follows the IP; real domains need a DNS change).
- **Data corruption:** the ledger is append-only, so reconstruct balances from `ledger_entries` (the balance/ledger agreement query is in `TestLedgerIsAppendOnly`).

## Recovery scenarios tested
- **Interrupted purchase** (provider timeout): stays `vending`, then reconciliation retries with the same reference and completes. Tested.
- **Delayed payment notification:** reconciliation `Verify` confirms it. Tested.
- **Duplicate notification:** a no-op. Tested.
- **Offline meter:** detected and alerted. Delivery waits for the meter to come back and acknowledge. Tested on the server side.
- **Network outage to the provider for more than 30 minutes:** goes to `needs_review` (logic implemented). The timing path is covered only by code review.
