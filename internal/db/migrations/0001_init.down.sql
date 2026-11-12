-- Rollback for 0001_init. Destroys all platform data: only for development or
-- a fresh failed install. Production rollback is restore-from-backup (see docs/RUNBOOK.md).
DROP TABLE IF EXISTS auto_topup_jobs, audit_logs, webhook_deliveries, webhook_endpoints, subscription_plans,
  support_messages, support_tickets, notifications, reward_grants, memberships, membership_tiers,
  share_recipients, idempotency_keys, energy_tokens, payments, vend_transactions, webhook_events,
  energy_entries, ledger_entries, balances, device_nonces, meter_alerts, meter_readings,
  meter_transfers, meter_links, meters, tariffs, properties, embed_codes, api_keys,
  refresh_tokens, users, tenant_branding, tenants CASCADE;
DROP FUNCTION IF EXISTS forbid_mutation(), app_bypass_rls(), app_tenant_id();
