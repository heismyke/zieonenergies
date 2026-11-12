-- Zieon white-label metering platform: initial schema.
-- Money is stored as BIGINT minor units (kobo). Energy is stored as BIGINT watt-hours (wh).
-- Tenant isolation: every tenant-owned table carries tenant_id and is protected by
-- PostgreSQL Row-Level Security (see the policies at the end of this file). The
-- application switches to the non-superuser role zieon_app inside every transaction,
-- so RLS applies even when the connecting role is a superuser.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zieon_app') THEN
    CREATE ROLE zieon_app NOLOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
END $$;
GRANT zieon_app TO CURRENT_USER;

-- Returns the tenant bound to the current transaction (NULL when unset).
CREATE OR REPLACE FUNCTION app_tenant_id() RETURNS uuid LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;

CREATE OR REPLACE FUNCTION app_bypass_rls() RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT coalesce(current_setting('app.bypass_rls', true), '') = 'on'
$$;

CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END $$;

-- ---------------------------------------------------------------------------
-- Tenants and branding
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug            text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,40}$'),
  name            text NOT NULL,
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
  is_platform     boolean NOT NULL DEFAULT false,
  features        jsonb NOT NULL DEFAULT '{}'::jsonb,
  settings        jsonb NOT NULL DEFAULT '{}'::jsonb,
  allowed_origins text[] NOT NULL DEFAULT '{}',
  vending_mode    text NOT NULL DEFAULT 'sandbox' CHECK (vending_mode IN ('sandbox','live')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tenants_single_platform ON tenants (is_platform) WHERE is_platform;

CREATE TABLE tenant_branding (
  tenant_id       uuid PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  display_name    text NOT NULL,
  logo_url        text NOT NULL DEFAULT '',
  logo_dark_url   text NOT NULL DEFAULT '',
  theme           jsonb NOT NULL DEFAULT '{}'::jsonb,
  font_family     text NOT NULL DEFAULT 'Satoshi',
  subdomain       text UNIQUE,
  custom_domain   text UNIQUE,
  support_email   text NOT NULL DEFAULT '',
  support_phone   text NOT NULL DEFAULT '',
  support_url     text NOT NULL DEFAULT '',
  currency        text NOT NULL DEFAULT 'NGN' CHECK (currency ~ '^[A-Z]{3}$'),
  locale          text NOT NULL DEFAULT 'en-NG',
  email_from_name text NOT NULL DEFAULT '',
  email_footer    text NOT NULL DEFAULT '',
  updated_at      timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------
CREATE TABLE users (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants(id),
  email            text NOT NULL,
  phone            text NOT NULL DEFAULT '',
  name             text NOT NULL DEFAULT '',
  password_hash    text,
  external_id      text,
  role             text NOT NULL CHECK (role IN ('platform_admin','tenant_admin','tenant_staff','customer')),
  status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  failed_logins    int NOT NULL DEFAULT 0,
  locked_until     timestamptz,
  last_login_at    timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_tenant_email ON users (tenant_id, lower(email));
CREATE UNIQUE INDEX users_tenant_external ON users (tenant_id, external_id) WHERE external_id IS NOT NULL;

CREATE TABLE refresh_tokens (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash  bytea NOT NULL UNIQUE,
  expires_at  timestamptz NOT NULL,
  revoked_at  timestamptz,
  replaced_by uuid,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  name         text NOT NULL,
  prefix       text NOT NULL UNIQUE,
  key_hash     bytea NOT NULL,
  scopes       text[] NOT NULL,
  created_by   uuid REFERENCES users(id),
  last_used_at timestamptz,
  expires_at   timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- Single-use, short-lived codes a partner backend mints for its signed-in user.
CREATE TABLE embed_codes (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash  bytea NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Properties and meters
-- ---------------------------------------------------------------------------
CREATE TABLE properties (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  owner_user_id uuid REFERENCES users(id),
  name          text NOT NULL,
  address       text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX properties_owner ON properties (tenant_id, owner_user_id);

-- Disco / provider catalogue and per-tenant tariffs.
CREATE TABLE tariffs (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenants(id),
  provider_code      text NOT NULL,
  provider_name      text NOT NULL,
  rate_kobo_per_kwh  bigint NOT NULL CHECK (rate_kobo_per_kwh > 0),
  service_fee_kobo   bigint NOT NULL DEFAULT 0 CHECK (service_fee_kobo >= 0),
  vat_bps            int NOT NULL DEFAULT 0 CHECK (vat_bps BETWEEN 0 AND 5000),
  min_purchase_kobo  bigint NOT NULL DEFAULT 10000,
  max_purchase_kobo  bigint NOT NULL DEFAULT 50000000,
  active             boolean NOT NULL DEFAULT true,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, provider_code)
);

-- A meter is either Zieon hardware registered by the platform (ownership='zieon')
-- or a third-party DisCo meter a customer linked (ownership='external').
CREATE TABLE meters (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid REFERENCES tenants(id),
  serial_number      text UNIQUE,
  meter_number       text NOT NULL,
  provider_code      text NOT NULL,
  ownership          text NOT NULL CHECK (ownership IN ('zieon','external')),
  meter_type         text NOT NULL DEFAULT 'prepaid' CHECK (meter_type IN ('prepaid','postpaid')),
  model              text NOT NULL DEFAULT '',
  comms_mode         text NOT NULL DEFAULT 'none' CHECK (comms_mode IN ('none','gsm','wifi','mqtt','lorawan')),
  capabilities       jsonb NOT NULL DEFAULT '{}'::jsonb,
  status             text NOT NULL DEFAULT 'inventory'
                       CHECK (status IN ('inventory','assigned','active','inactive','decommissioned')),
  firmware_version   text NOT NULL DEFAULT '',
  device_secret_enc  bytea,
  connectivity       text NOT NULL DEFAULT 'unknown' CHECK (connectivity IN ('unknown','online','offline')),
  last_seen_at       timestamptz,
  customer_name      text NOT NULL DEFAULT '',
  address            text NOT NULL DEFAULT '',
  property_id        uuid REFERENCES properties(id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
-- Zieon hardware is globally unique. External DisCo meters are unique per tenant
-- so one tenant's link never blocks (or reveals) the same meter in another tenant.
CREATE UNIQUE INDEX meters_zieon_number ON meters (provider_code, meter_number) WHERE ownership = 'zieon';
CREATE UNIQUE INDEX meters_external_number ON meters (tenant_id, provider_code, meter_number) WHERE ownership = 'external';
ALTER TABLE meters ADD CONSTRAINT meters_zieon_serial CHECK (ownership <> 'zieon' OR serial_number IS NOT NULL);
ALTER TABLE meters ADD CONSTRAINT meters_external_tenant CHECK (ownership <> 'external' OR tenant_id IS NOT NULL);
CREATE INDEX meters_tenant ON meters (tenant_id);

-- Which customer may operate which meter, with their own label ("Home", "Office").
CREATE TABLE meter_links (
  id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id                 uuid NOT NULL REFERENCES tenants(id),
  meter_id                  uuid NOT NULL REFERENCES meters(id),
  user_id                   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label                     text NOT NULL,
  is_owner                  boolean NOT NULL DEFAULT true,
  low_balance_threshold_wh  bigint NOT NULL DEFAULT 10000,
  low_balance_notified_at   timestamptz,
  auto_topup_enabled        boolean NOT NULL DEFAULT false,
  auto_topup_threshold_wh   bigint NOT NULL DEFAULT 0,
  auto_topup_amount_kobo    bigint NOT NULL DEFAULT 0,
  created_at                timestamptz NOT NULL DEFAULT now(),
  UNIQUE (meter_id, user_id)
);
-- A meter has at most one owning customer at a time.
CREATE UNIQUE INDEX meter_links_one_owner ON meter_links (meter_id) WHERE is_owner;

CREATE TABLE meter_transfers (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  meter_id      uuid NOT NULL REFERENCES meters(id),
  from_user_id  uuid NOT NULL REFERENCES users(id),
  to_user_id    uuid NOT NULL REFERENCES users(id),
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','cancelled')),
  reason        text NOT NULL DEFAULT '',
  decided_by    uuid REFERENCES users(id),
  decided_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX meter_transfers_one_pending ON meter_transfers (meter_id) WHERE status = 'pending';

CREATE TABLE meter_readings (
  id               bigserial PRIMARY KEY,
  tenant_id        uuid NOT NULL REFERENCES tenants(id),
  meter_id         uuid NOT NULL REFERENCES meters(id),
  recorded_at      timestamptz NOT NULL,
  credit_wh        bigint,
  cumulative_wh    bigint,
  voltage_dv       int,
  current_ma       int,
  power_status     text,
  tamper           boolean NOT NULL DEFAULT false,
  source           text NOT NULL DEFAULT 'device',
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX meter_readings_meter_time ON meter_readings (meter_id, recorded_at DESC);

CREATE TABLE meter_alerts (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  meter_id    uuid NOT NULL REFERENCES meters(id),
  kind        text NOT NULL,
  message     text NOT NULL,
  resolved_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- Replay protection for signed device requests.
CREATE TABLE device_nonces (
  meter_id   uuid NOT NULL REFERENCES meters(id),
  nonce      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (meter_id, nonce)
);

-- ---------------------------------------------------------------------------
-- Money: balances + immutable ledger
-- ---------------------------------------------------------------------------
-- account_type: wallet (user naira wallet), reward (user promotional balance),
-- reward_pool (partner-funded pool, account_id = tenant id), clearing (vending sales).
CREATE TABLE balances (
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  account_type text NOT NULL CHECK (account_type IN ('wallet','reward','reward_pool','clearing')),
  account_id   uuid NOT NULL,
  balance_kobo bigint NOT NULL DEFAULT 0,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_type, account_id),
  CHECK (account_type = 'clearing' OR balance_kobo >= 0)
);

CREATE TABLE ledger_entries (
  id            bigserial PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  account_type  text NOT NULL,
  account_id    uuid NOT NULL,
  amount_kobo   bigint NOT NULL CHECK (amount_kobo <> 0),
  balance_after bigint NOT NULL,
  kind          text NOT NULL,
  reference     text NOT NULL,
  memo          text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_type, account_id, kind, reference)
);
CREATE INDEX ledger_entries_account ON ledger_entries (account_type, account_id, id DESC);
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Energy movements tracked by the platform per meter (watt-hours).
CREATE TABLE energy_entries (
  id         bigserial PRIMARY KEY,
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  meter_id   uuid NOT NULL REFERENCES meters(id),
  wh         bigint NOT NULL CHECK (wh <> 0),
  kind       text NOT NULL,
  reference  text NOT NULL,
  memo       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (meter_id, kind, reference)
);
CREATE TRIGGER energy_entries_immutable BEFORE UPDATE OR DELETE ON energy_entries
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Payments and vending
-- ---------------------------------------------------------------------------
CREATE TABLE payments (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenants(id),
  user_id            uuid NOT NULL REFERENCES users(id),
  purpose            text NOT NULL CHECK (purpose IN ('wallet_fund','purchase')),
  provider           text NOT NULL,
  method             text NOT NULL,
  reference          text NOT NULL UNIQUE,
  amount_kobo        bigint NOT NULL CHECK (amount_kobo > 0),
  currency           text NOT NULL DEFAULT 'NGN',
  status             text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','succeeded','failed','expired')),
  checkout_url       text NOT NULL DEFAULT '',
  provider_payload   jsonb NOT NULL DEFAULT '{}'::jsonb,
  vend_transaction_id uuid,
  confirmed_at       timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_pending ON payments (status, created_at) WHERE status = 'pending';

-- Inbound provider webhooks, de-duplicated by event id.
CREATE TABLE webhook_events (
  provider    text NOT NULL,
  event_id    text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  payload     jsonb NOT NULL,
  PRIMARY KEY (provider, event_id)
);

CREATE TABLE vend_transactions (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenants(id),
  user_id            uuid NOT NULL REFERENCES users(id),
  meter_id           uuid NOT NULL REFERENCES meters(id),
  status             text NOT NULL CHECK (status IN (
                       'payment_pending','paid','vending','vended','delivered',
                       'payment_failed','vend_failed','needs_review','refunded')),
  route              text NOT NULL,
  provider           text NOT NULL,
  payment_method     text NOT NULL,
  amount_kobo        bigint NOT NULL CHECK (amount_kobo > 0),
  wallet_kobo        bigint NOT NULL DEFAULT 0,
  reward_kobo        bigint NOT NULL DEFAULT 0,
  external_kobo      bigint NOT NULL DEFAULT 0,
  fee_kobo           bigint NOT NULL DEFAULT 0,
  vat_kobo           bigint NOT NULL DEFAULT 0,
  energy_value_kobo  bigint NOT NULL,
  rate_kobo_per_kwh  bigint NOT NULL,
  energy_wh          bigint NOT NULL CHECK (energy_wh > 0),
  provider_reference text,
  failure_reason     text NOT NULL DEFAULT '',
  attempts           int NOT NULL DEFAULT 0,
  sandbox            boolean NOT NULL,
  source             text NOT NULL DEFAULT 'customer',
  vended_at          timestamptz,
  delivered_at       timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK (wallet_kobo + reward_kobo + external_kobo = amount_kobo)
);
CREATE INDEX vend_tx_user ON vend_transactions (tenant_id, user_id, created_at DESC);
CREATE INDEX vend_tx_status ON vend_transactions (status, updated_at);
ALTER TABLE payments ADD CONSTRAINT payments_vend_tx_fk
  FOREIGN KEY (vend_transaction_id) REFERENCES vend_transactions(id);

-- Tokens issued by a vending route or by Spark Share. The token itself is
-- encrypted at rest; token_hash allows lookup without decrypting.
CREATE TABLE energy_tokens (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id),
  meter_id            uuid NOT NULL REFERENCES meters(id),
  issued_to_user_id   uuid NOT NULL REFERENCES users(id),
  source              text NOT NULL CHECK (source IN ('purchase','share')),
  vend_transaction_id uuid REFERENCES vend_transactions(id),
  token_enc           bytea NOT NULL,
  token_hash          bytea NOT NULL UNIQUE,
  token_last4         text NOT NULL,
  energy_wh           bigint NOT NULL CHECK (energy_wh > 0),
  note                text NOT NULL DEFAULT '',
  status              text NOT NULL DEFAULT 'issued' CHECK (status IN ('issued','submitted','delivered','void')),
  sandbox             boolean NOT NULL,
  submitted_at        timestamptz,
  delivered_at        timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX energy_tokens_meter ON energy_tokens (meter_id, status);

CREATE TABLE idempotency_keys (
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  principal     text NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, principal, key)
);

CREATE TABLE share_recipients (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  meter_number  text NOT NULL,
  last_note     text NOT NULL DEFAULT '',
  last_wh       bigint NOT NULL DEFAULT 0,
  last_shared_at timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, meter_number)
);

-- ---------------------------------------------------------------------------
-- Membership tiers and partner-funded rewards
-- ---------------------------------------------------------------------------
CREATE TABLE membership_tiers (
  id                         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id                  uuid NOT NULL REFERENCES tenants(id),
  code                       text NOT NULL,
  name                       text NOT NULL,
  rank                       int NOT NULL DEFAULT 0,
  cashback_bps               int NOT NULL DEFAULT 0 CHECK (cashback_bps BETWEEN 0 AND 10000),
  max_cashback_kobo          bigint NOT NULL DEFAULT 0 CHECK (max_cashback_kobo >= 0),
  monthly_cashback_cap_kobo  bigint NOT NULL DEFAULT 0 CHECK (monthly_cashback_cap_kobo >= 0),
  fixed_reward_kobo          bigint NOT NULL DEFAULT 0 CHECK (fixed_reward_kobo >= 0),
  fixed_reward_min_purchase_kobo bigint NOT NULL DEFAULT 0,
  monthly_credit_kobo        bigint NOT NULL DEFAULT 0 CHECK (monthly_credit_kobo >= 0),
  min_purchase_kobo          bigint NOT NULL DEFAULT 0,
  reward_validity_days       int NOT NULL DEFAULT 90 CHECK (reward_validity_days > 0),
  active                     boolean NOT NULL DEFAULT true,
  created_at                 timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, code)
);

CREATE TABLE memberships (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tier_id    uuid NOT NULL REFERENCES membership_tiers(id),
  valid_from timestamptz NOT NULL DEFAULT now(),
  valid_to   timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX memberships_one_open ON memberships (user_id) WHERE valid_to IS NULL;

CREATE TABLE reward_grants (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenants(id),
  user_id        uuid NOT NULL REFERENCES users(id),
  tier_id        uuid REFERENCES membership_tiers(id),
  kind           text NOT NULL CHECK (kind IN ('cashback','fixed_reward','monthly_credit','refund')),
  source_ref     text NOT NULL,
  amount_kobo    bigint NOT NULL CHECK (amount_kobo > 0),
  remaining_kobo bigint NOT NULL CHECK (remaining_kobo >= 0),
  expires_at     timestamptz NOT NULL,
  revoked_at     timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, user_id, kind, source_ref)
);
CREATE INDEX reward_grants_open ON reward_grants (user_id, expires_at) WHERE remaining_kobo > 0;

-- Auto top-up requests raised by telemetry, executed by the worker.
CREATE TABLE auto_topup_jobs (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id),
  link_id             uuid NOT NULL REFERENCES meter_links(id) ON DELETE CASCADE,
  status              text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','done','failed')),
  vend_transaction_id uuid REFERENCES vend_transactions(id),
  error               text NOT NULL DEFAULT '',
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX auto_topup_one_pending ON auto_topup_jobs (link_id) WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- Notifications, support, subscriptions
-- ---------------------------------------------------------------------------
CREATE TABLE notifications (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  user_id    uuid REFERENCES users(id) ON DELETE CASCADE,
  title      text NOT NULL,
  message    text NOT NULL,
  critical   boolean NOT NULL DEFAULT false,
  kind       text NOT NULL DEFAULT 'info',
  read_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user ON notifications (tenant_id, user_id, created_at DESC);

CREATE TABLE support_tickets (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  user_id     uuid NOT NULL REFERENCES users(id),
  subject     text NOT NULL,
  status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_progress','resolved','closed')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE support_messages (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  ticket_id   uuid NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
  author_id   uuid NOT NULL REFERENCES users(id),
  from_staff  boolean NOT NULL DEFAULT false,
  body        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE subscription_plans (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  code        text NOT NULL,
  name        text NOT NULL,
  price_kobo  bigint NOT NULL DEFAULT 0 CHECK (price_kobo >= 0),
  period      text NOT NULL DEFAULT 'month',
  features    text[] NOT NULL DEFAULT '{}',
  rank        int NOT NULL DEFAULT 0,
  active      boolean NOT NULL DEFAULT true,
  UNIQUE (tenant_id, code)
);

-- ---------------------------------------------------------------------------
-- Partner webhooks (outbound) and audit
-- ---------------------------------------------------------------------------
CREATE TABLE webhook_endpoints (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  url         text NOT NULL CHECK (url ~ '^https://'),
  secret_enc  bytea NOT NULL,
  events      text[] NOT NULL,
  active      boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id),
  endpoint_id     uuid NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
  event_id        uuid NOT NULL,
  event_type      text NOT NULL,
  payload         jsonb NOT NULL,
  status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','failed')),
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_error      text NOT NULL DEFAULT '',
  delivered_at    timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (endpoint_id, event_id)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

CREATE TABLE audit_logs (
  id            bigserial PRIMARY KEY,
  tenant_id     uuid REFERENCES tenants(id),
  actor_user_id uuid,
  actor_type    text NOT NULL,
  action        text NOT NULL,
  target_type   text NOT NULL DEFAULT '',
  target_id     text NOT NULL DEFAULT '',
  metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
  ip            text NOT NULL DEFAULT '',
  request_id    text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_tenant ON audit_logs (tenant_id, id DESC);
CREATE TRIGGER audit_logs_immutable BEFORE UPDATE OR DELETE ON audit_logs
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Privileges and Row-Level Security
-- ---------------------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO zieon_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zieon_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zieon_app;
REVOKE UPDATE, DELETE ON ledger_entries, energy_entries, audit_logs FROM zieon_app;

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'tenant_branding','users','refresh_tokens','api_keys','embed_codes','properties','tariffs',
    'meter_links','meter_transfers','meter_readings','meter_alerts','balances','ledger_entries',
    'energy_entries','payments','vend_transactions','energy_tokens','idempotency_keys',
    'share_recipients','membership_tiers','memberships','reward_grants','notifications',
    'support_tickets','support_messages','subscription_plans','webhook_endpoints','webhook_deliveries','auto_topup_jobs']
  LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
      USING (app_bypass_rls() OR tenant_id = app_tenant_id())
      WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id())$p$, t);
  END LOOP;
END $$;

-- Meters may be unassigned inventory (tenant_id NULL), visible only to the platform.
ALTER TABLE meters ENABLE ROW LEVEL SECURITY;
ALTER TABLE meters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON meters
  USING (app_bypass_rls() OR tenant_id = app_tenant_id())
  WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id());

-- Audit logs: tenants can read their own rows; anyone in the app role may append.
ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_read ON audit_logs FOR SELECT USING (app_bypass_rls() OR tenant_id = app_tenant_id());
CREATE POLICY audit_append ON audit_logs FOR INSERT WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id() OR tenant_id IS NULL);
