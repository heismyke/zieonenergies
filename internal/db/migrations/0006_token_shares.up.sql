-- One-time links (shown as a QR) that reveal an order's token to whoever opens
-- them first. Only a hash of the code is stored; the link stops working once
-- the token is revealed, when it expires, or when a newer link replaces it.
CREATE TABLE token_shares (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id),
  vend_transaction_id uuid NOT NULL REFERENCES vend_transactions(id),
  created_by          uuid NOT NULL REFERENCES users(id),
  code_hash           bytea NOT NULL UNIQUE,
  created_at          timestamptz NOT NULL DEFAULT now(),
  expires_at          timestamptz NOT NULL,
  revealed_at         timestamptz
);
CREATE INDEX token_shares_vend ON token_shares (vend_transaction_id);
GRANT SELECT, INSERT, UPDATE ON token_shares TO zieon_app;
ALTER TABLE token_shares ENABLE ROW LEVEL SECURITY;
ALTER TABLE token_shares FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON token_shares
  USING (app_bypass_rls() OR tenant_id = app_tenant_id())
  WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id());
