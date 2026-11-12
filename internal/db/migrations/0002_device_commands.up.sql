-- Downlink commands to meters (delivered over MQTT by the device gateway).
-- The API enqueues rows; the gateway publishes them to connected devices and
-- records the device's signed acknowledgement. Devices must treat command ids
-- as idempotent because delivery is at-least-once.
CREATE TABLE device_commands (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  meter_id    uuid NOT NULL REFERENCES meters(id),
  type        text NOT NULL CHECK (type IN ('credit_token','relay','sync','ping')),
  params      jsonb NOT NULL DEFAULT '{}'::jsonb,
  status      text NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued','sent','accepted','executed','rejected','failed','expired')),
  attempts    int NOT NULL DEFAULT 0,
  result      text NOT NULL DEFAULT '',
  created_by  uuid,
  expires_at  timestamptz NOT NULL,
  sent_at     timestamptz,
  acked_at    timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX device_commands_due ON device_commands (meter_id, created_at) WHERE status IN ('queued','sent');
CREATE INDEX device_commands_meter ON device_commands (tenant_id, meter_id, created_at DESC);

GRANT SELECT, INSERT, UPDATE ON device_commands TO zieon_app;
ALTER TABLE device_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_commands FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON device_commands
  USING (app_bypass_rls() OR tenant_id = app_tenant_id())
  WITH CHECK (app_bypass_rls() OR tenant_id = app_tenant_id());
