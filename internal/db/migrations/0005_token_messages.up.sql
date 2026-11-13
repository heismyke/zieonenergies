-- Token receipts sent outside the app (email, SMS, WhatsApp). A row is queued
-- with the order and sent once the token exists; it holds no token itself
-- (the sender decrypts it from energy_tokens at send time).
CREATE TABLE outbound_messages (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id),
  vend_transaction_id uuid NOT NULL REFERENCES vend_transactions(id),
  channel             text NOT NULL CHECK (channel IN ('email','sms','whatsapp')),
  destination         text NOT NULL,
  status              text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed','skipped')),
  attempts            int NOT NULL DEFAULT 0,
  next_attempt_at     timestamptz NOT NULL DEFAULT now(),
  provider_ref        text NOT NULL DEFAULT '',
  last_error          text NOT NULL DEFAULT '',
  created_at          timestamptz NOT NULL DEFAULT now(),
  sent_at             timestamptz,
  UNIQUE (vend_transaction_id, channel)
);
CREATE INDEX outbound_messages_due ON outbound_messages (next_attempt_at) WHERE status = 'pending';
GRANT SELECT, INSERT, UPDATE ON outbound_messages TO zieon_app;
ALTER TABLE outbound_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbound_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbound_messages
  USING (app_bypass_rls() OR tenant_id = app_tenant_id())
  WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id());
