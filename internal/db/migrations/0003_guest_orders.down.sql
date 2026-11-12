DROP TABLE IF EXISTS guest_orders;
UPDATE payments SET status='succeeded' WHERE status IN ('refund_pending','refunded');
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments ADD CONSTRAINT payments_status_check CHECK (status IN ('pending','succeeded','failed','expired'));
