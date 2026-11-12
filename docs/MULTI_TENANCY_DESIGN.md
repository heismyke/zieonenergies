# Multi-Tenancy Design

## Model
- A **tenant** is a partner organisation, such as `pce-energy` or `red-real-estate`.
- Exactly one tenant has `is_platform = true`. This is Zieon itself. Its admins have role `platform_admin`, and the same tenant also runs the first-party Zenerlink consumer brand.
- **Users** belong to exactly one tenant.
  - Email is unique per tenant, not globally. The same person can be a PCE customer and a Red investor with independent wallets (tested).
  - Partner-SSO users also carry a stable `external_id` that is unique per tenant.
- **Roles:**

| Role | Scope | Can |
|---|---|---|
| `platform_admin` | Platform tenant only | Provision or suspend tenants, register meter inventory, assign meters to tenants, set tariffs and vending mode, fund reward pools, resolve `needs_review` purchases, read audit logs across tenants. Every view inside a tenant writes an audit row. |
| `tenant_admin` | Own tenant | Branding, feature flags, approved origins, team, API keys, webhooks, tiers, memberships, meter-to-customer assignment, transfer decisions, broadcasts. |
| `tenant_staff` | Own tenant | Read customers, meters, transactions, reports and support. Reply to tickets. No credentials, keys or configuration. |
| `customer` | Own data | Own meters, wallet, purchases, shares, benefits, tickets. |

**Who owns what:**
- Tariffs are **platform-managed**. A tenant admin cannot change regulated pricing; rewards are the partner's lever.
- `vending_mode = live` cannot be set until an authorised live vending provider is configured (enforced, and tested).

## Where tenant context comes from
| Caller | Source of tenant id | Trust |
|---|---|---|
| Signed-in user | `tid` claim in an HS256 JWT signed by Zieon | Verified on every request. The user and tenant are re-checked as active. |
| Partner backend | API key row (`api_keys.tenant_id`) | Key is hashed (SHA-256), scoped, revocable. |
| Meter device | `meters.tenant_id` for the authenticated serial | HMAC with a per-device secret (AES-GCM at rest). |
| Unauthenticated (login, sign-up, branding) | `X-Tenant` header, `?tenant=`, or custom domain/subdomain | A **public identifier only**. It never grants access. After login the token is authoritative and a mismatching header is rejected with 403. |

## Enforcement layers
1. **Handler layer:** role and scope checks (`s.user(r, roles…)`, `s.apiKey(r, scope)`).
2. **Service layer:** every query carries `tenant_id = $1`. Customer queries also join `meter_links` on `user_id`. Unknown or foreign ids return **404, not 403**, so the existence of another tenant's records is never confirmed.
3. **Database layer: PostgreSQL RLS.**
   - `ENABLE` and `FORCE ROW LEVEL SECURITY` are set on all 29 tenant tables.
   - The policy is: `USING (app_bypass_rls() OR tenant_id = app_tenant_id()) WITH CHECK (same)`.
   - `db.Store.Tenant()` runs `SET LOCAL ROLE zieon_app` (NOSUPERUSER, NOBYPASSRLS) and `set_config('app.tenant_id', …, true)`. RLS therefore applies even when the pool connects as a superuser.
   - `db.Store.System()` sets `app.bypass_rls = on`. It is used only for platform operations, pre-auth credential lookups (refresh token, API key prefix, device serial, payment reference for webhooks) and workers. Each call site is a reviewed, narrow lookup.
4. **Constraints:**
   - Global unique Zieon meter serial and number.
   - External DisCo meters are unique per `(tenant, provider, number)`, so one tenant's link never blocks or reveals another's.
   - One owner per meter, and one pending transfer per meter.

## Tests proving isolation
`internal/httpapi/e2e_test.go: TestTwoTenantIsolation` provisions PCE Energy and Red Real Estate through the platform API, then asserts all of the following:
- **Branding** differs per tenant and is served correctly to each.
- **Cross-tenant reads by id** (meter, purchase) return 404.
- **Cross-tenant purchase and token redemption** are refused: 404 for a foreign meter, 400 for a foreign token on one's own meter.
- **Admin lists** contain only own-tenant customers, meters and transactions.
- **Cross-tenant admin actions** (membership, meter assignment) return 404.
- **Tenant header spoofing** returns 403.
- **Same-email users** in two tenants have separate credentials and wallets.
- **Tenant admins** cannot reach platform endpoints.
- **Feature flags** apply per tenant (benefits off for PCE).
- **RLS:** inside a PCE transaction, `SELECT count(*)` with **no WHERE** sees only PCE rows, and inserting a row with Red's `tenant_id` is refused by the policy.

## Operational notes
- **Suspending a tenant** (`PATCH /v1/platform/tenants/{id} {"status":"suspended"}`) immediately blocks logins and invalidates existing access tokens on their next request (tested).
- **Moving a meter between tenants** requires it to have no customer links. This prevents orphaned customer history.
