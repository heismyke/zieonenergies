# Production deployment (buy-only zenerlink.com)

Production is a separate stack from staging: own database, own secrets, `APP_ENV=production`
(sandbox payments and sandbox vending are disabled in code).

| | Staging | Production |
|---|---|---|
| Web | staging.zenerlink.com (full dashboard; `?mode=vend` previews buy-only) | zenerlink.com, www.zenerlink.com (buy-only) |
| API | zieon-api.198.199.65.142.sslip.io | api.zenerlink.com |
| Server dir / compose project | `~/apps/zieon` / `zieon-prod` | `~/apps/zieon-live` / `zieon-live` |
| Payments / vending | sandbox / VTpass sandbox | Paystack live / VTpass live |

## Prerequisites
1. **DNS** (at the zenerlink.com DNS provider):
   - `zenerlink.com` and `www` → the records Vercel shows when you add the domains to the `zenerlink-frontend` project.
   - `api.zenerlink.com` → `A 198.199.65.142`.
2. **Paystack live** secret key (business verified); webhook URL `https://api.zenerlink.com/v1/webhooks/payments/paystack`.
3. **VTpass live** account (verified, wallet funded), API key + public/secret keys, auth type "API Keys", electricity products whitelisted.

## Deploy
```bash
ssh myke@198.199.65.142
mkdir -p ~/apps/zieon-live && cd ~/apps/zieon-live
# upload the repo at the release commit (git archive | ssh … tar -x)
cp deploy/env.live.example .env.live && chmod 600 .env.live   # fill in; generate NEW keys, never reuse staging's
docker compose --env-file .env.live -f docker-compose.live.yml up -d --build
docker compose --env-file .env.live -f docker-compose.live.yml exec -T api /app/api bootstrap > bootstrap.txt; chmod 600 bootstrap.txt
```
`bootstrap` creates the buy-only Zenerlink tenant and sets it live when the live VTpass route is configured
(it prints a warning and stays in sandbox otherwise).

Caddy (`~/apps/reverse-proxy/Caddyfile`; back up, validate, then reload):
```
api.zenerlink.com {
    reverse_proxy zieon-live-api:8080
}
```

## Verify before announcing
1. `curl https://api.zenerlink.com/healthz` → 200; `GET /v1/branding` shows `"sandbox": false`.
2. On zenerlink.com: verify your own meter (real name appears), buy the DisCo minimum with a real card,
   load the token on the meter keypad.
3. Paystack dashboard shows the payment; VTpass dashboard shows the vend; `GET /v1/public/orders/{id}` shows `delivered`/`vended`.
4. Force a failure (e.g. temporarily unfunded VTpass wallet → `018`): order shows "refunded"; Paystack shows the refund.

## Operate
- Backups: run `deploy/backup.sh` adapted to `docker-compose.live.yml` nightly, and copy off-host.
- Stuck purchases (`needs_review`) never refund automatically; resolve via
  `POST /v1/platform/tenants/{id}/transactions/{txn}/resolve` after checking VTpass's records.
- Keep the VTpass wallet funded (low balance → purchases fail with `018` and are refunded to the card).
