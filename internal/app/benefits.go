package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/db"
)

// Rewards are promotional value funded by the partner's reward pool. They are
// held in a separate "reward" balance and spent as a funding leg of a
// purchase. They never change the tariff, fee or VAT of the electricity sold.

type Tier struct {
	ID                         uuid.UUID `json:"id"`
	Code                       string    `json:"code"`
	Name                       string    `json:"name"`
	Rank                       int       `json:"rank"`
	CashbackBps                int       `json:"cashback_bps"`
	MaxCashbackKobo            int64     `json:"max_cashback_kobo"`
	MonthlyCashbackCapKobo     int64     `json:"monthly_cashback_cap_kobo"`
	FixedRewardKobo            int64     `json:"fixed_reward_kobo"`
	FixedRewardMinPurchaseKobo int64     `json:"fixed_reward_min_purchase_kobo"`
	MonthlyCreditKobo          int64     `json:"monthly_credit_kobo"`
	MinPurchaseKobo            int64     `json:"min_purchase_kobo"`
	RewardValidityDays         int       `json:"reward_validity_days"`
	Active                     bool      `json:"active"`
}

const tierCols = `id, code, name, rank, cashback_bps, max_cashback_kobo, monthly_cashback_cap_kobo, fixed_reward_kobo,
	fixed_reward_min_purchase_kobo, monthly_credit_kobo, min_purchase_kobo, reward_validity_days, active`

func scanTier(row pgx.Row) (Tier, error) {
	var t Tier
	err := row.Scan(&t.ID, &t.Code, &t.Name, &t.Rank, &t.CashbackBps, &t.MaxCashbackKobo, &t.MonthlyCashbackCapKobo, &t.FixedRewardKobo,
		&t.FixedRewardMinPurchaseKobo, &t.MonthlyCreditKobo, &t.MinPurchaseKobo, &t.RewardValidityDays, &t.Active)
	return t, notFoundIfNoRows(err)
}

func validateTier(t Tier) error {
	switch {
	case t.Code == "" || len(t.Code) > 40 || strings.ContainsAny(t.Code, " /"):
		return errf(http.StatusBadRequest, "invalid_tier", "code is required (no spaces, max 40)")
	case strings.TrimSpace(t.Name) == "":
		return errf(http.StatusBadRequest, "invalid_tier", "name is required")
	case t.CashbackBps < 0 || t.CashbackBps > 5000:
		return errf(http.StatusBadRequest, "invalid_tier", "cashback_bps must be 0-5000 (0-50%%)")
	case t.CashbackBps > 0 && t.MaxCashbackKobo <= 0:
		return errf(http.StatusBadRequest, "invalid_tier", "max_cashback_kobo is required when cashback is enabled")
	case t.MaxCashbackKobo < 0 || t.MonthlyCashbackCapKobo < 0 || t.FixedRewardKobo < 0 || t.MonthlyCreditKobo < 0 || t.MinPurchaseKobo < 0:
		return errf(http.StatusBadRequest, "invalid_tier", "amounts cannot be negative")
	}
	return nil
}

func (s *Service) UpsertTier(ctx context.Context, tenant uuid.UUID, t Tier) (Tier, error) {
	if t.RewardValidityDays == 0 {
		t.RewardValidityDays = 90
	}
	if err := validateTier(t); err != nil {
		return Tier{}, err
	}
	var out Tier
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		out, err = scanTier(tx.QueryRow(ctx, `INSERT INTO membership_tiers (tenant_id, code, name, rank, cashback_bps, max_cashback_kobo,
			monthly_cashback_cap_kobo, fixed_reward_kobo, fixed_reward_min_purchase_kobo, monthly_credit_kobo, min_purchase_kobo, reward_validity_days, active)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (tenant_id, code) DO UPDATE SET name=EXCLUDED.name, rank=EXCLUDED.rank, cashback_bps=EXCLUDED.cashback_bps,
			max_cashback_kobo=EXCLUDED.max_cashback_kobo, monthly_cashback_cap_kobo=EXCLUDED.monthly_cashback_cap_kobo,
			fixed_reward_kobo=EXCLUDED.fixed_reward_kobo, fixed_reward_min_purchase_kobo=EXCLUDED.fixed_reward_min_purchase_kobo,
			monthly_credit_kobo=EXCLUDED.monthly_credit_kobo, min_purchase_kobo=EXCLUDED.min_purchase_kobo,
			reward_validity_days=EXCLUDED.reward_validity_days, active=EXCLUDED.active
			RETURNING `+tierCols, tenant, t.Code, t.Name, t.Rank, t.CashbackBps, t.MaxCashbackKobo, t.MonthlyCashbackCapKobo, t.FixedRewardKobo,
			t.FixedRewardMinPurchaseKobo, t.MonthlyCreditKobo, t.MinPurchaseKobo, t.RewardValidityDays, t.Active))
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "benefits.tier.upsert", TargetType: "tier", TargetID: out.ID.String(),
			Metadata: map[string]any{"code": t.Code, "cashback_bps": t.CashbackBps, "max_cashback_kobo": t.MaxCashbackKobo, "monthly_credit_kobo": t.MonthlyCreditKobo}})
	})
	return out, err
}

func (s *Service) Tiers(ctx context.Context, tenant uuid.UUID) ([]Tier, error) {
	out := []Tier{}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+tierCols+` FROM membership_tiers WHERE tenant_id=$1 ORDER BY rank, code`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTier(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// SetMembership closes any open membership and opens one on tierCode ("" removes).
func (s *Service) SetMembership(ctx context.Context, tenant, userID uuid.UUID, tierCode string) error {
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var role string
		if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 AND tenant_id=$2`, userID, tenant).Scan(&role); err != nil {
			return errf(http.StatusNotFound, "customer_not_found", "customer not found")
		}
		if role != auth.RoleCustomer {
			return errf(http.StatusBadRequest, "invalid_customer", "memberships apply to customers only")
		}
		var tierID *uuid.UUID
		if tierCode != "" {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM membership_tiers WHERE tenant_id=$1 AND code=$2 AND active`, tenant, tierCode).Scan(&id); err != nil {
				return errf(http.StatusNotFound, "tier_not_found", "no active tier %q", tierCode)
			}
			tierID = &id
			var current *uuid.UUID
			_ = tx.QueryRow(ctx, `SELECT tier_id FROM memberships WHERE user_id=$1 AND valid_to IS NULL`, userID).Scan(&current)
			if current != nil && *current == id {
				return nil // unchanged
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE memberships SET valid_to=now() WHERE user_id=$1 AND valid_to IS NULL`, userID); err != nil {
			return err
		}
		if tierID != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, tier_id) VALUES ($1,$2,$3)`, tenant, userID, *tierID); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "benefits.membership.set", TargetType: "user", TargetID: userID.String(),
			Metadata: map[string]any{"tier": tierCode}})
	})
}

func activeTier(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (*Tier, error) {
	t, err := scanTier(tx.QueryRow(ctx, `SELECT `+prefixed("t.", tierCols)+` FROM memberships m JOIN membership_tiers t ON t.id=m.tier_id
		WHERE m.user_id=$1 AND m.valid_from <= now() AND (m.valid_to IS NULL OR m.valid_to > now()) AND t.active
		ORDER BY m.valid_from DESC LIMIT 1`, userID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func prefixed(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// grant draws from the partner pool into the customer's reward balance. If
// the pool cannot cover the full amount the grant is reduced to what is
// available (never overdrawn). Duplicate (kind, source_ref) grants are refused
// by a unique constraint, which is the primary duplicate-claim defence.
func grant(ctx context.Context, tx pgx.Tx, tenant, user uuid.UUID, tier *uuid.UUID, kind, sourceRef string, amount int64, validity time.Duration) (int64, error) {
	if amount <= 0 {
		return 0, nil
	}
	pool, err := balance(ctx, tx, AcctRewardPool, tenant)
	if err != nil {
		return 0, err
	}
	amount = min(amount, pool)
	if amount <= 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO reward_grants (tenant_id, user_id, tier_id, kind, source_ref, amount_kobo, remaining_kobo, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$6,$7) ON CONFLICT DO NOTHING`, tenant, user, tier, kind, sourceRef, amount, time.Now().Add(validity))
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, nil // already granted
	}
	if err := transfer(ctx, tx, tenant, AcctRewardPool, tenant, AcctReward, user, amount, kind, sourceRef, ""); err != nil {
		return 0, err
	}
	return amount, emit(ctx, tx, tenant, "reward.granted", map[string]any{"user_id": user, "kind": kind, "amount_kobo": amount, "source_ref": sourceRef})
}

// grantPurchaseRewards applies tier cashback and fixed rewards to a vended purchase.
// Only the customer-paid legs (wallet + external) earn cashback, never rewards spent.
func (s *Service) grantPurchaseRewards(ctx context.Context, tx pgx.Tx, v vendRow) (int64, error) {
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((features->>'benefits')::boolean, false) FROM tenants WHERE id=$1`, v.tenant).Scan(&enabled); err != nil || !enabled {
		return 0, err
	}
	tier, err := activeTier(ctx, tx, v.user)
	if err != nil || tier == nil {
		return 0, err
	}
	paid := v.wallet + v.external
	if paid < tier.MinPurchaseKobo {
		return 0, nil
	}
	validity := time.Duration(tier.RewardValidityDays) * 24 * time.Hour
	var total int64
	if tier.CashbackBps > 0 {
		cb := min(paid*int64(tier.CashbackBps)/10000, tier.MaxCashbackKobo)
		if tier.MonthlyCashbackCapKobo > 0 {
			var month int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount_kobo),0) FROM reward_grants WHERE user_id=$1 AND kind='cashback'
				AND created_at >= date_trunc('month', now()) AND revoked_at IS NULL`, v.user).Scan(&month); err != nil {
				return 0, err
			}
			cb = min(cb, max(0, tier.MonthlyCashbackCapKobo-month))
		}
		n, err := grant(ctx, tx, v.tenant, v.user, &tier.ID, "cashback", v.id.String(), cb, validity)
		if err != nil {
			return 0, err
		}
		total += n
	}
	if tier.FixedRewardKobo > 0 && paid >= tier.FixedRewardMinPurchaseKobo {
		n, err := grant(ctx, tx, v.tenant, v.user, &tier.ID, "fixed_reward", v.id.String(), tier.FixedRewardKobo, validity)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// consumeRewards spends reward balance FIFO by expiry.
func consumeRewards(ctx context.Context, tx pgx.Tx, tenant, user uuid.UUID, amount int64, ref string) error {
	rows, err := tx.Query(ctx, `SELECT id, remaining_kobo FROM reward_grants WHERE user_id=$1 AND remaining_kobo > 0 AND revoked_at IS NULL
		AND expires_at > now() ORDER BY expires_at FOR UPDATE`, user)
	if err != nil {
		return err
	}
	type g struct {
		id  uuid.UUID
		rem int64
	}
	var gs []g
	for rows.Next() {
		var x g
		if err := rows.Scan(&x.id, &x.rem); err != nil {
			rows.Close()
			return err
		}
		gs = append(gs, x)
	}
	rows.Close()
	left := amount
	for _, x := range gs {
		if left == 0 {
			break
		}
		take := min(left, x.rem)
		if _, err := tx.Exec(ctx, `UPDATE reward_grants SET remaining_kobo = remaining_kobo - $2 WHERE id=$1`, x.id, take); err != nil {
			return err
		}
		left -= take
	}
	if left > 0 {
		return ErrInsufficient
	}
	return transfer(ctx, tx, tenant, AcctReward, user, AcctClearing, tenant, amount, "reward_spend", ref, "Rewards applied to purchase")
}

func refundRewards(ctx context.Context, tx pgx.Tx, tenant, user uuid.UUID, amount int64, ref string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO reward_grants (tenant_id, user_id, kind, source_ref, amount_kobo, remaining_kobo, expires_at)
		VALUES ($1,$2,'refund',$3,$4,$4, now() + interval '30 days')`, tenant, user, ref, amount); err != nil {
		return err
	}
	return transfer(ctx, tx, tenant, AcctClearing, tenant, AcctReward, user, amount, "reward_refund", ref, "Rewards returned")
}

// ClaimMonthlyCredit grants the tier's monthly allowance once per calendar month.
func (s *Service) ClaimMonthlyCredit(ctx context.Context, p auth.Principal, t Tenant) (int64, error) {
	if !t.Enabled("benefits") {
		return 0, ErrFeatureOff
	}
	var amount int64
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		tier, err := activeTier(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if tier == nil || tier.MonthlyCreditKobo == 0 {
			return errf(http.StatusConflict, "not_eligible", "your membership does not include a monthly electricity credit")
		}
		period := time.Now().UTC().Format("2006-01")
		ref := "monthly:" + period
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reward_grants WHERE user_id=$1 AND kind='monthly_credit' AND source_ref=$2)`,
			p.UserID, ref).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errf(http.StatusConflict, "already_claimed", "this month's credit has already been claimed")
		}
		amount, err = grant(ctx, tx, p.TenantID, p.UserID, &tier.ID, "monthly_credit", ref, tier.MonthlyCreditKobo, time.Duration(tier.RewardValidityDays)*24*time.Hour)
		if err != nil {
			return err
		}
		if amount == 0 {
			return errf(http.StatusServiceUnavailable, "pool_empty", "rewards are temporarily unavailable; please try later")
		}
		return notify(ctx, tx, p.TenantID, &p.UserID, "Monthly credit added", fmt.Sprintf("₦%.2f electricity credit is ready to use.", float64(amount)/100), "reward", false)
	})
	if db.IsUniqueViolation(err) {
		return 0, errf(http.StatusConflict, "already_claimed", "this month's credit has already been claimed")
	}
	return amount, err
}

type RewardGrant struct {
	Kind          string    `json:"kind"`
	SourceRef     string    `json:"source_ref"`
	AmountKobo    int64     `json:"amount_kobo"`
	RemainingKobo int64     `json:"remaining_kobo"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
}

type BenefitsSummary struct {
	Enabled             bool          `json:"enabled"`
	Tier                *Tier         `json:"tier"`
	RewardBalanceKobo   int64         `json:"reward_balance_kobo"`
	MonthlyCreditClaimed bool         `json:"monthly_credit_claimed"`
	History             []RewardGrant `json:"history"`
}

func (s *Service) Benefits(ctx context.Context, p auth.Principal, t Tenant) (BenefitsSummary, error) {
	out := BenefitsSummary{Enabled: t.Enabled("benefits"), History: []RewardGrant{}}
	if !out.Enabled {
		return out, nil
	}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		if out.Tier, err = activeTier(ctx, tx, p.UserID); err != nil {
			return err
		}
		if out.RewardBalanceKobo, err = balance(ctx, tx, AcctReward, p.UserID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reward_grants WHERE user_id=$1 AND kind='monthly_credit' AND source_ref=$2)`,
			p.UserID, "monthly:"+time.Now().UTC().Format("2006-01")).Scan(&out.MonthlyCreditClaimed); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind, source_ref, amount_kobo, remaining_kobo, expires_at, created_at FROM reward_grants
			WHERE user_id=$1 ORDER BY created_at DESC LIMIT 50`, p.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g RewardGrant
			if err := rows.Scan(&g.Kind, &g.SourceRef, &g.AmountKobo, &g.RemainingKobo, &g.ExpiresAt, &g.CreatedAt); err != nil {
				return err
			}
			out.History = append(out.History, g)
		}
		return rows.Err()
	})
	return out, err
}

// DepositRewardPool records partner funding received by Zieon (platform admin only).
func (s *Service) DepositRewardPool(ctx context.Context, tenant uuid.UUID, amount int64, reference string) (int64, error) {
	if amount <= 0 || strings.TrimSpace(reference) == "" {
		return 0, errf(http.StatusBadRequest, "invalid_input", "positive amount_kobo and a reference are required")
	}
	var bal int64
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		err := transfer(ctx, tx, tenant, AcctClearing, tenant, AcctRewardPool, tenant, amount, "pool_deposit", reference, "Partner reward funding")
		if errors.Is(err, errDuplicatePost) {
			return errf(http.StatusConflict, "duplicate_reference", "a deposit with this reference was already recorded")
		}
		if err != nil {
			return err
		}
		if bal, err = balance(ctx, tx, AcctRewardPool, tenant); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "platform.reward_pool.deposit", TargetType: "tenant", TargetID: tenant.String(),
			Metadata: map[string]any{"amount_kobo": amount, "reference": reference}})
	})
	return bal, err
}

func (s *Service) RewardPoolBalance(ctx context.Context, tenant uuid.UUID) (int64, error) {
	var b int64
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		b, err = balance(ctx, tx, AcctRewardPool, tenant)
		return err
	})
	return b, err
}

// ExpireRewards debits unspent expired grants back to the partner pool.
func (s *Service) ExpireRewards(ctx context.Context) (int, error) {
	n := 0
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, user_id, remaining_kobo FROM reward_grants
			WHERE remaining_kobo > 0 AND expires_at <= now() AND revoked_at IS NULL LIMIT 500 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type g struct {
			id, tenant, user uuid.UUID
			rem              int64
		}
		var gs []g
		for rows.Next() {
			var x g
			if err := rows.Scan(&x.id, &x.tenant, &x.user, &x.rem); err != nil {
				rows.Close()
				return err
			}
			gs = append(gs, x)
		}
		rows.Close()
		for _, x := range gs {
			if _, err := tx.Exec(ctx, `UPDATE reward_grants SET remaining_kobo=0 WHERE id=$1`, x.id); err != nil {
				return err
			}
			if err := transfer(ctx, tx, x.tenant, AcctReward, x.user, AcctRewardPool, x.tenant, x.rem, "reward_expired", x.id.String(), "Rewards expired"); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
