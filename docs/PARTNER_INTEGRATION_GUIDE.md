# Partner Integration Guide

Base URL (pilot): `https://zieon-api.198.199.65.142.sslip.io`. All paths are versioned under `/v1`, and all bodies are JSON.

Errors look like this:
```json
{"error": {"code": "insufficient_funds", "message": "…"}, "request_id": "…"}
```
Unknown JSON fields are rejected with `400 invalid_json`.

## 1. Onboarding (done by Zieon)
1. **Zieon provisions your tenant:**
   ```
   POST /v1/platform/tenants
   {slug, name, branding{…}, features{…}, allowed_origins[], admin{email,name,password}}
   ```
2. **You sign in as tenant admin:**
   ```
   POST /v1/auth/login
   X-Tenant: <slug>
   ```
3. **You configure:**
   - Branding: `PATCH /v1/admin/branding`
     ```json
     {"display_name":"PCE Power","logo_url":"https://…","theme":{"primary":"#1F7AE0"},"support_email":"…"}
     ```
     Theme tokens are `primary`, `secondary`, `background`, `surface`, `panel`, `border`, `success`, `danger`, `text` and `text_muted`. Each must be a `#RRGGBB` colour.
   - Approved origins: `PATCH /v1/admin/settings {"allowed_origins":["https://app.pce.example"]}`.
   - API key: `POST /v1/admin/api-keys {"name":"backend","scopes":["sessions:write","transactions:read"]}`. **The `secret` is shown once.**
   - Webhook: `POST /v1/admin/webhooks {"url":"https://…","events":["purchase.vended","wallet.funded"]}`. **The `secret` is shown once.**

## 2. Embedding the hosted electricity module (recommended)
Your backend authenticates your user, then:
```http
POST /v1/partner/sessions
Authorization: Bearer zk_xxxx.yyyy
{"external_user_id":"pce-user-42","email":"u42@pce.example","name":"Uche","return_origin":"https://app.pce.example"}

201 {"code":"zec_…","expires_at":"…","embed_url":"https://<module>/?embed=1&tenant=pce-energy#code=zec_…","user_id":"…"}
```
Load `embed_url` in an iframe or webview **within 2 minutes**.

What happens next:
- The module exchanges the code once (`POST /v1/embed/exchange`) for a ≤15-minute session, with no refresh token.
- The code travels in the URL fragment, so it never reaches server logs.
- `external_user_id` is your stable id. The same id always maps to the same Zieon customer.

**Security rules:**
- Never call `/v1/partner/*` from a browser or mobile app. API keys are server-side only.
- Request a new code when the session ends. The module shows "Your session has ended".
- The page hosting the module must send `Content-Security-Policy: frame-ancestors <your approved origins>`. Tell Zieon your origins.

## 3. Server-to-server API
| Endpoint | Scope | Purpose |
|---|---|---|
| `POST /v1/partner/sessions` | `sessions:write` | SSO code (above) |
| `GET /v1/partner/customers/{external_id}` | `customers:read` | Look up a customer |
| `PUT /v1/partner/customers/{external_id}/membership {"tier_code":"gold"}` | `memberships:write` | Sync membership tier (`""` removes it) |
| `GET /v1/partner/transactions?external_user_id=&status=&limit=` | `transactions:read` | Vending history (tokens are never returned to partners) |
| `GET /v1/partner/meters?status=` | `meters:read` | Your assigned meters |

Rate limit: 600 requests/min per key (burst 100). Exceeding it returns `429 rate_limited`.

## 4. Webhooks
Headers:
```
Zieon-Event: purchase.vended
Zieon-Delivery: <uuid>
Zieon-Signature: t=<unix>,v1=<hex>
```
- Verify `v1 == HMAC_SHA256(secret, "<t>.<raw body>")` and reject if `|now − t| > 5 min`.
- Respond with 2xx within 10 s. Anything else is retried with exponential backoff (8 attempts over about 20 h).
- Deliveries are **at-least-once**, so de-duplicate on body `id`.

Events:
- `purchase.vended`, `purchase.refunded`, `purchase.payment_failed`, `purchase.needs_review`
- `wallet.funded`, `token.delivered`
- `meter.low_balance`, `meter.offline`, `meter.tamper`
- `reward.granted`

Example body:
```json
{"id":"8f…","type":"purchase.vended","api_version":"v1","created_at":"…",
 "data":{"purchase_id":"…","user_id":"…","meter_id":"…","amount_kobo":500000,"energy_kwh":25,"sandbox":true}}
```
Verification in Node:
```js
const [t, v1] = header.split(',').map(p => p.split('=')[1]);
const ok = crypto.timingSafeEqual(Buffer.from(v1,'hex'),
  crypto.createHmac('sha256', secret).update(`${t}.${rawBody}`).digest());
```

## 5. Membership benefits (Red Real Estate pattern)
- **Configure tiers:**
  ```
  PUT /v1/admin/tiers
  {"code":"gold","name":"Gold","cashback_bps":500,"max_cashback_kobo":250000,"monthly_cashback_cap_kobo":750000,"monthly_credit_kobo":200000,"reward_validity_days":90,"active":true}
  ```
- **Fund the pool:** wire funds to Zieon. Zieon records the deposit against your pool. Rewards stop (they are never overdrawn) when the pool is empty.
- **Assign tiers:** via the partner API or `PUT /v1/admin/customers/{id}/membership`.
- **What customers see:** cashback appears as a separate reward balance and can be applied to future purchases (`use_rewards: true`). The tariff and kWh are unchanged.

## 6. Customer API (used by the hosted module and first-party apps)

**Auth:**
- `POST /v1/auth/register`, `/login`, `/refresh`, `/logout`
- `GET /v1/me`, `PATCH /v1/me`

**Catalogue:**
- `GET /v1/branding`, `GET /v1/tariffs`, `GET /v1/plans`, `POST /v1/quotes`

**Wallet:**
- `GET /v1/wallet`
- `POST /v1/wallet/fund` *(needs Idempotency-Key)*

**Meters:**
- `GET|POST /v1/meters`, `POST /v1/meters/validate`
- `GET|PATCH|DELETE /v1/meters/{id}`
- `GET /v1/meters/{id}/consumption`, `GET /v1/meters/{id}/readings`
- `POST /v1/meters/{id}/transfer`

**Purchases and tokens:**
- `POST /v1/purchases` *(Idempotency-Key)*, `GET /v1/purchases[/{id}]`
- `GET /v1/tokens`, `POST /v1/tokens/redeem`

**Sharing:**
- `POST /v1/shares` *(Idempotency-Key)*
- `GET|POST /v1/recipients`, `DELETE /v1/recipients/{meter_number}`

**Account:**
- `GET /v1/activity`, `GET /v1/notifications`, `POST /v1/notifications/{id}/read`
- `GET|POST /v1/properties`
- `GET /v1/benefits`, `POST /v1/benefits/monthly-credit`
- `GET|POST /v1/support/tickets`, `GET /v1/support/tickets/{id}`, `POST /v1/support/tickets/{id}/messages`

**Idempotency:**
- Send a unique `Idempotency-Key` (≤128 chars) per logical operation.
- A replay with the same body returns the original response.
- The same key with a different body returns `422`.
- A request still in flight returns `409`.
