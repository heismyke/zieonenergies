-- Email updates sign-ups from a tenant's public site. Signing up again is a
-- no-op; unsubscribed_at is set when someone opts out.
CREATE TABLE subscribers (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id),
  email           text NOT NULL,
  source          text NOT NULL DEFAULT 'website',
  created_at      timestamptz NOT NULL DEFAULT now(),
  unsubscribed_at timestamptz,
  UNIQUE (tenant_id, email)
);
GRANT SELECT, INSERT, UPDATE ON subscribers TO zieon_app;
ALTER TABLE subscribers ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscribers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscribers
  USING (app_bypass_rls() OR tenant_id = app_tenant_id())
  WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id());
