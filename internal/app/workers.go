package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/secure"
	"github.com/heismyke/zieonenergies/internal/vending"
)

// RunWorkers runs periodic jobs until ctx is cancelled. Each job uses
// FOR UPDATE SKIP LOCKED, so several API instances can run workers safely.
func (s *Service) RunWorkers(ctx context.Context) {
	jobs := []struct {
		name  string
		every time.Duration
		fn    func(context.Context) error
	}{
		{"webhooks", 5 * time.Second, s.DeliverWebhooks},
		{"reconcile_vends", 30 * time.Second, s.ReconcileVends},
		{"reconcile_payments", time.Minute, s.ReconcilePayments},
		{"auto_topup", 30 * time.Second, s.RunAutoTopups},
		{"meter_offline", time.Minute, s.DetectOfflineMeters},
		{"expire_rewards", 10 * time.Minute, func(ctx context.Context) error { _, err := s.ExpireRewards(ctx); return err }},
		{"expire_commands", time.Minute, s.ExpireCommands},
		{"cleanup", time.Hour, s.Cleanup},
	}
	for _, j := range jobs {
		go func() {
			t := time.NewTicker(j.every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					jctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					if err := s.safeRun(jctx, j.fn); err != nil {
						s.Log.Error("worker failed", "job", j.name, "err", err)
					}
					cancel()
				}
			}
		}()
	}
}

func (s *Service) safeRun(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx)
}

// ReconcileVends resolves purchases stuck in "vending" (crash, timeout,
// provider outage) by asking the provider what happened to our reference.
func (s *Service) ReconcileVends(ctx context.Context) error {
	type stuck struct {
		id, tenant uuid.UUID
		provider   string
		attempts   int
		age        time.Duration
	}
	var list []stuck
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, provider, attempts, now() - created_at FROM vend_transactions
			WHERE status='vending' AND updated_at < now() - interval '2 minutes' ORDER BY updated_at LIMIT 50 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x stuck
			if err := rows.Scan(&x.id, &x.tenant, &x.provider, &x.attempts, &x.age); err != nil {
				return err
			}
			list = append(list, x)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, x := range list {
		vp := s.Vending[x.provider]
		if vp == nil {
			continue
		}
		req, err := s.VendRequest(ctx, x.tenant, x.id)
		if err != nil {
			s.Log.Error("reconcile: load request", "txn", x.id, "err", err)
			continue
		}
		res, lerr := vp.Lookup(ctx, req)
		switch {
		case lerr == nil:
			s.applyVendResult(ctx, x.tenant, x.id, res, nil)
		case errors.Is(lerr, vending.ErrNotFound) && x.attempts < 3:
			s.executeVend(ctx, x.tenant, x.id) // provider never saw it: retry with the same reference
		case errors.Is(lerr, vending.ErrNotFound):
			s.applyVendResult(ctx, x.tenant, x.id, vending.Result{}, vending.ErrRejected)
		case x.age > 30*time.Minute:
			// Provider cannot tell us; a human must check before money moves.
			_ = s.DB.Tenant(ctx, x.tenant, func(tx pgx.Tx) error {
				v, err := lockVend(ctx, tx, x.id)
				if err != nil || v.status != StVending {
					return err
				}
				return s.markReview(ctx, tx, v, "vend outcome unknown after 30 minutes: "+lerr.Error())
			})
		}
	}
	return nil
}

// ReconcilePayments verifies pending payments whose webhook never arrived.
func (s *Service) ReconcilePayments(ctx context.Context) error {
	type pending struct {
		ref, provider string
		age           time.Duration
	}
	var list []pending
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT reference, provider, now() - created_at FROM payments WHERE status='pending'
			AND created_at < now() - interval '10 minutes' ORDER BY created_at LIMIT 50`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.ref, &p.provider, &p.age); err != nil {
				return err
			}
			list = append(list, p)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, p := range list {
		pp := s.Payments[p.provider]
		if pp == nil {
			continue
		}
		v, err := pp.Verify(ctx, p.ref)
		if err != nil {
			s.Log.Warn("payment verify", "ref", p.ref, "err", err)
			continue
		}
		switch {
		case v.Status == "succeeded":
			err = s.ConfirmPayment(ctx, p.provider, p.ref, v.AmountKobo)
		case v.Status == "failed":
			err = s.FailPayment(ctx, p.ref, "payment failed")
		case p.age > 24*time.Hour:
			err = s.FailPayment(ctx, p.ref, "payment expired")
		}
		if err != nil {
			s.Log.Error("payment reconcile", "ref", p.ref, "err", err)
		}
	}
	return nil
}

// RunAutoTopups executes queued auto top-ups from the customer's wallet.
func (s *Service) RunAutoTopups(ctx context.Context) error {
	type job struct {
		id, tenant, link, user, meter uuid.UUID
		amount                        int64
	}
	var jobs []job
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT j.id, j.tenant_id, j.link_id, l.user_id, l.meter_id, l.auto_topup_amount_kobo
			FROM auto_topup_jobs j JOIN meter_links l ON l.id=j.link_id WHERE j.status='pending' AND l.auto_topup_enabled
			ORDER BY j.created_at LIMIT 20 FOR UPDATE OF j SKIP LOCKED`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j job
			if err := rows.Scan(&j.id, &j.tenant, &j.link, &j.user, &j.meter, &j.amount); err != nil {
				return err
			}
			jobs = append(jobs, j)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, j := range jobs {
		t, err := s.TenantByID(ctx, j.tenant)
		if err != nil {
			continue
		}
		p := auth.Principal{Kind: "user", UserID: j.user, TenantID: j.tenant, Role: auth.RoleCustomer}
		sysCtx := WithMeta(ctx, RequestMeta{Principal: auth.Principal{Kind: "system"}})
		pu, perr := s.Purchase(sysCtx, p, t, PurchaseInput{MeterID: j.meter, AmountKobo: j.amount, PaymentMethod: "wallet"})
		status, msg := "done", ""
		var vendID *uuid.UUID
		if perr != nil {
			status, msg = "failed", perr.Error()
		} else {
			vendID = &pu.ID
		}
		_ = s.DB.Tenant(ctx, j.tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE auto_topup_jobs SET status=$2, error=$3, vend_transaction_id=$4, updated_at=now() WHERE id=$1`,
				j.id, status, msg, vendID); err != nil {
				return err
			}
			if perr != nil {
				return notify(ctx, tx, j.tenant, &j.user, "Auto top-up failed", "We could not top up your meter automatically. Check your wallet balance.", "auto_topup", true)
			}
			return nil
		})
	}
	return nil
}

// DetectOfflineMeters flags telemetry meters that stopped reporting.
func (s *Service) DetectOfflineMeters(ctx context.Context) error {
	return s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE meters SET connectivity='offline', updated_at=now()
			WHERE connectivity='online' AND last_seen_at < now() - interval '30 minutes' AND tenant_id IS NOT NULL
			RETURNING id, tenant_id, serial_number`)
		if err != nil {
			return err
		}
		type m struct {
			id, tenant uuid.UUID
			serial     *string
		}
		var ms []m
		for rows.Next() {
			var x m
			if err := rows.Scan(&x.id, &x.tenant, &x.serial); err != nil {
				rows.Close()
				return err
			}
			ms = append(ms, x)
		}
		rows.Close()
		for _, x := range ms {
			if _, err := tx.Exec(ctx, `INSERT INTO meter_alerts (tenant_id, meter_id, kind, message) VALUES ($1,$2,'offline','Meter stopped reporting')`, x.tenant, x.id); err != nil {
				return err
			}
			if err := emit(ctx, tx, x.tenant, "meter.offline", map[string]any{"meter_id": x.id, "serial_number": x.serial}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) Cleanup(ctx context.Context) error {
	return s.DB.System(ctx, func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM device_nonces WHERE created_at < now() - interval '1 day'`,
			`DELETE FROM idempotency_keys WHERE created_at < now() - interval '7 days'`,
			`DELETE FROM embed_codes WHERE expires_at < now() - interval '1 day'`,
			`DELETE FROM refresh_tokens WHERE expires_at < now() - interval '7 days'`,
			`DELETE FROM webhook_events WHERE received_at < now() - interval '90 days'`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Outbound webhook delivery
// ---------------------------------------------------------------------------

const maxWebhookAttempts = 8

// webhookBackoff: 30s, 1m, 2m, 4m ... capped at 6h.
func webhookBackoff(attempt int) time.Duration {
	d := 30 * time.Second << max(0, attempt-1)
	return min(d, 6*time.Hour)
}

// DeliverWebhooks posts due deliveries with a Zieon-Signature header.
func (s *Service) DeliverWebhooks(ctx context.Context) error {
	type d struct {
		id, tenant uuid.UUID
		url        string
		secret     []byte
		eventType  string
		payload    []byte
		attempts   int
	}
	var ds []d
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT d.id, d.tenant_id, e.url, e.secret_enc, d.event_type, d.payload, d.attempts
			FROM webhook_deliveries d JOIN webhook_endpoints e ON e.id=d.endpoint_id
			WHERE d.status='pending' AND d.next_attempt_at <= now() AND e.active ORDER BY d.next_attempt_at LIMIT 50 FOR UPDATE OF d SKIP LOCKED`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var x d
			if err := rows.Scan(&x.id, &x.tenant, &x.url, &x.secret, &x.eventType, &x.payload, &x.attempts); err != nil {
				rows.Close()
				return err
			}
			ds = append(ds, x)
		}
		rows.Close()
		// Claim the batch so a concurrent worker skips it while we deliver.
		for _, x := range ds {
			if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at=now() + interval '2 minutes' WHERE id=$1`, x.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, x := range ds {
		secret, err := s.Keys.Decrypt(x.secret)
		derr := err
		if err == nil {
			derr = s.postWebhook(ctx, x.url, secret, x.eventType, x.id, x.payload)
		}
		attempts := x.attempts + 1
		_ = s.DB.System(ctx, func(tx pgx.Tx) error {
			if derr == nil {
				_, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status='delivered', attempts=$2, delivered_at=now(), last_error='' WHERE id=$1`, x.id, attempts)
				return err
			}
			status := "pending"
			if attempts >= maxWebhookAttempts {
				status = "failed"
			}
			_, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status=$2, attempts=$3, last_error=$4, next_attempt_at=now() + $5::interval WHERE id=$1`,
				x.id, status, attempts, truncate(derr.Error(), 500), fmt.Sprintf("%d seconds", int(webhookBackoff(attempts).Seconds())))
			return err
		})
	}
	return nil
}

func (s *Service) postWebhook(ctx context.Context, url string, secret []byte, eventType string, deliveryID uuid.UUID, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Zieon-Webhooks/1")
	req.Header.Set("Zieon-Event", eventType)
	req.Header.Set("Zieon-Delivery", deliveryID.String())
	req.Header.Set("Zieon-Signature", secure.SignWebhook(secret, s.Now(), payload))
	res, err := s.webhookClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("endpoint returned %d", res.StatusCode)
	}
	return nil
}

// webhookClient refuses to connect to private, loopback or link-local
// addresses at dial time, defeating DNS-rebinding SSRF against internal services.
func (s *Service) webhookClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("webhook destination address not allowed")
		}
		return nil
	}}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
