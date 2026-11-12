-- Guest checkout (production "buy electricity" flow). A guest order is a
-- normal vend transaction owned by a passwordless customer record; the buyer
-- proves ownership with a random order key (only its hash is stored).
CREATE TABLE guest_orders (
  vend_transaction_id uuid PRIMARY KEY REFERENCES vend_transactions(id),
  tenant_id           uuid NOT NULL REFERENCES tenants(id),
  key_hash            bytea NOT NULL,
  email               text NOT NULL,
  phone               text NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT ON guest_orders TO zieon_app;
ALTER TABLE guest_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE guest_orders FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON guest_orders
  USING (app_bypass_rls() OR tenant_id = app_tenant_id())
  WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id());

-- Card refunds for failed guest purchases go back to the card, not a wallet.
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check
  CHECK (status IN ('pending','succeeded','failed','expired','refund_pending','refunded'));
