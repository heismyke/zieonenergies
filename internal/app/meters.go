package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/db"
	"github.com/heismyke/zieonenergies/internal/secure"
)

// Capabilities describe what a meter can actually do. Nothing is assumed:
// a meter without "telemetry" never reports readings, and remote unit
// transfers require "remote_debit" on the sending meter.
type Capabilities struct {
	Telemetry    bool `json:"telemetry"`
	RemoteCredit bool `json:"remote_credit"`
	RemoteDebit  bool `json:"remote_debit"`
	Relay        bool `json:"relay"`
	Tamper       bool `json:"tamper"`
	Keypad       bool `json:"keypad"`
}

type Meter struct {
	ID              uuid.UUID    `json:"id"`
	LinkID          uuid.UUID    `json:"link_id,omitempty"`
	Label           string       `json:"label"`
	ProviderCode    string       `json:"provider_code"`
	ProviderName    string       `json:"provider_name"`
	MeterNumber     string       `json:"meter_number"`
	SerialNumber    *string      `json:"serial_number,omitempty"`
	CustomerName    string       `json:"customer_name"`
	Address         string       `json:"address"`
	MeterType       string       `json:"meter_type"`
	Ownership       string       `json:"ownership"`
	Status          string       `json:"status"`
	CommsMode       string       `json:"comms_mode"`
	Capabilities    Capabilities `json:"capabilities"`
	FirmwareVersion string       `json:"firmware_version"`
	Connectivity    string       `json:"connectivity"`
	LastSeenAt      *time.Time   `json:"last_seen_at"`
	PropertyID      *uuid.UUID   `json:"property_id"`
	IsOwner         bool         `json:"is_owner"`
	// KwhReserve is the best available credit figure. BalanceSource says where
	// it came from: "device" (last meter report) or "platform_ledger" (units
	// delivered and shared through Zieon, without consumption data).
	KwhReserve      float64    `json:"kwh_reserve"`
	BalanceSource   string     `json:"balance_source"`
	DeviceCreditKwh *float64   `json:"device_credit_kwh"`
	PlatformKwh     float64    `json:"platform_kwh"`
	LowBalanceKwh   float64    `json:"low_balance_threshold_kwh"`
	AutoTopup       AutoTopup  `json:"auto_topup"`
	OwnerUserID     *uuid.UUID `json:"owner_user_id,omitempty"`
}

type AutoTopup struct {
	Enabled      bool    `json:"enabled"`
	ThresholdKwh float64 `json:"threshold_kwh"`
	AmountKobo   int64   `json:"amount_kobo"`
}

// meterSelect joins the per-user link; $2 is the user id (or NULL for admin views).
const meterSelect = `SELECT m.id, coalesce(l.id, '00000000-0000-0000-0000-000000000000'::uuid), coalesce(l.label, m.meter_number),
	m.provider_code, coalesce(t.provider_name, m.provider_code), m.meter_number, m.serial_number, m.customer_name, m.address,
	m.meter_type, m.ownership, m.status, m.comms_mode, m.capabilities, m.firmware_version, m.connectivity, m.last_seen_at,
	m.property_id, coalesce(l.is_owner, false),
	(SELECT r.credit_wh FROM meter_readings r WHERE r.meter_id=m.id AND r.credit_wh IS NOT NULL ORDER BY r.recorded_at DESC LIMIT 1),
	coalesce((SELECT sum(e.wh) FROM energy_entries e WHERE e.meter_id=m.id), 0),
	coalesce(l.low_balance_threshold_wh, 10000), coalesce(l.auto_topup_enabled,false), coalesce(l.auto_topup_threshold_wh,0),
	coalesce(l.auto_topup_amount_kobo,0),
	(SELECT o.user_id FROM meter_links o WHERE o.meter_id=m.id AND o.is_owner)
	FROM meters m
	LEFT JOIN meter_links l ON l.meter_id=m.id AND l.user_id=$2
	LEFT JOIN tariffs t ON t.tenant_id=m.tenant_id AND t.provider_code=m.provider_code `

func scanMeter(row pgx.Row) (Meter, error) {
	var m Meter
	var caps []byte
	var deviceWh *int64
	var platformWh, lowWh, autoWh int64
	err := row.Scan(&m.ID, &m.LinkID, &m.Label, &m.ProviderCode, &m.ProviderName, &m.MeterNumber, &m.SerialNumber, &m.CustomerName,
		&m.Address, &m.MeterType, &m.Ownership, &m.Status, &m.CommsMode, &caps, &m.FirmwareVersion, &m.Connectivity, &m.LastSeenAt,
		&m.PropertyID, &m.IsOwner, &deviceWh, &platformWh, &lowWh, &m.AutoTopup.Enabled, &autoWh, &m.AutoTopup.AmountKobo, &m.OwnerUserID)
	if err != nil {
		return m, notFoundIfNoRows(err)
	}
	_ = json.Unmarshal(caps, &m.Capabilities)
	m.PlatformKwh = WhToKwh(platformWh)
	m.LowBalanceKwh = WhToKwh(lowWh)
	m.AutoTopup.ThresholdKwh = WhToKwh(autoWh)
	if deviceWh != nil && m.Capabilities.Telemetry {
		v := WhToKwh(*deviceWh)
		m.DeviceCreditKwh = &v
		m.KwhReserve, m.BalanceSource = v, "device"
	} else {
		m.KwhReserve, m.BalanceSource = m.PlatformKwh, "platform_ledger"
	}
	if m.LinkID == uuid.Nil {
		m.Label = m.MeterNumber
	}
	return m, nil
}

func (s *Service) MyMeters(ctx context.Context, p auth.Principal) ([]Meter, error) {
	var out []Meter
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, meterSelect+`WHERE m.tenant_id=$1 AND l.id IS NOT NULL AND m.status <> 'decommissioned' ORDER BY l.created_at`,
			p.TenantID, p.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMeter(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// myMeter loads a meter the caller is linked to; otherwise ErrNotFound (never
// "forbidden", so meter ids from other customers or tenants are not confirmed).
func myMeter(ctx context.Context, tx pgx.Tx, p auth.Principal, meterID uuid.UUID) (Meter, error) {
	return scanMeter(tx.QueryRow(ctx, meterSelect+`WHERE m.tenant_id=$1 AND m.id=$3 AND l.id IS NOT NULL`, p.TenantID, p.UserID, meterID))
}

func (s *Service) MyMeter(ctx context.Context, p auth.Principal, meterID uuid.UUID) (Meter, error) {
	var m Meter
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		m, err = myMeter(ctx, tx, p, meterID)
		return err
	})
	return m, err
}

var meterNumberRe = regexp.MustCompile(`^[0-9]{10,20}$`)

type MeterValidation struct {
	Verified     bool   `json:"verified"`
	ProviderCode string `json:"provider_code"`
	MeterNumber  string `json:"meter_number"`
	CustomerName string `json:"customer_name"`
	Address      string `json:"address"`
	MeterType    string `json:"meter_type"`
	Source       string `json:"source"`
	Message      string `json:"message"`
}

// ValidateMeter looks a meter up before purchase or linking. Zieon meters are
// verified from our registry. DisCo customer lookup needs an aggregator
// integration that does not exist yet, so external meters return verified=false.
func (s *Service) ValidateMeter(ctx context.Context, p auth.Principal, providerCode, meterNumber string) (MeterValidation, error) {
	providerCode = strings.ToUpper(strings.TrimSpace(providerCode))
	meterNumber = strings.TrimSpace(meterNumber)
	v := MeterValidation{ProviderCode: providerCode, MeterNumber: meterNumber}
	if !meterNumberRe.MatchString(meterNumber) {
		return v, errf(http.StatusBadRequest, "invalid_meter_number", "meter number must be 10-20 digits")
	}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT customer_name, address, meter_type FROM meters
			WHERE tenant_id=$1 AND provider_code=$2 AND meter_number=$3 AND status IN ('assigned','active')`,
			p.TenantID, providerCode, meterNumber).Scan(&v.CustomerName, &v.Address, &v.MeterType)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			v.Verified, v.Source = true, "zieon_registry"
		}
		return err
	})
	if err != nil || v.Verified {
		return v, err
	}
	if providerCode == "ZIEON" {
		v.Source, v.Message = "zieon_registry", "This Zieon meter is not registered to your organisation."
		return v, nil
	}
	v.Source = "unverified"
	v.Message = "DisCo customer lookup is not yet connected; confirm the meter number and enter the customer details."
	return v, nil
}

type LinkMeterInput struct {
	ProviderCode string     `json:"provider_code"`
	MeterNumber  string     `json:"meter_number"`
	Label        string     `json:"label"`
	CustomerName string     `json:"customer_name"`
	Address      string     `json:"address"`
	MeterType    string     `json:"meter_type"`
	PropertyID   *uuid.UUID `json:"property_id"`
}

func (s *Service) LinkMeter(ctx context.Context, p auth.Principal, t Tenant, in LinkMeterInput) (Meter, error) {
	in.ProviderCode = strings.ToUpper(strings.TrimSpace(in.ProviderCode))
	in.MeterNumber = strings.TrimSpace(in.MeterNumber)
	in.Label = strings.TrimSpace(in.Label)
	if !meterNumberRe.MatchString(in.MeterNumber) {
		return Meter{}, errf(http.StatusBadRequest, "invalid_meter_number", "meter number must be 10-20 digits")
	}
	if in.Label == "" || len(in.Label) > 40 {
		return Meter{}, errf(http.StatusBadRequest, "invalid_label", "label is required (max 40 chars)")
	}
	if in.MeterType == "" {
		in.MeterType = "prepaid"
	}
	if in.MeterType != "prepaid" && in.MeterType != "postpaid" {
		return Meter{}, errf(http.StatusBadRequest, "invalid_meter_type", "meter_type must be prepaid or postpaid")
	}
	var out Meter
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tariffs WHERE tenant_id=$1 AND provider_code=$2 AND active)`,
			p.TenantID, in.ProviderCode).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errf(http.StatusBadRequest, "unknown_provider", "provider %q is not available", in.ProviderCode)
		}
		if in.PropertyID != nil {
			if err := ownsProperty(ctx, tx, p, *in.PropertyID); err != nil {
				return err
			}
		}
		var meterID uuid.UUID
		var ownership, status string
		err := tx.QueryRow(ctx, `SELECT id, ownership, status FROM meters WHERE tenant_id=$1 AND provider_code=$2 AND meter_number=$3`,
			p.TenantID, in.ProviderCode, in.MeterNumber).Scan(&meterID, &ownership, &status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if in.ProviderCode == "ZIEON" {
				return errf(http.StatusNotFound, "meter_not_registered", "this Zieon meter is not registered to your organisation")
			}
			if !t.Enabled("external_meters") {
				return ErrFeatureOff
			}
			err = tx.QueryRow(ctx, `INSERT INTO meters (tenant_id, meter_number, provider_code, ownership, meter_type, status, customer_name, address, property_id)
				VALUES ($1,$2,$3,'external',$4,'active',$5,$6,$7) RETURNING id`,
				p.TenantID, in.MeterNumber, in.ProviderCode, in.MeterType, in.CustomerName, in.Address, in.PropertyID).Scan(&meterID)
			if err != nil {
				return err
			}
			ownership = "external"
		case err != nil:
			return err
		default:
			if status == "inactive" || status == "decommissioned" || status == "inventory" {
				return errf(http.StatusConflict, "meter_unavailable", "this meter is not active")
			}
		}
		var owner *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM meter_links WHERE meter_id=$1 AND is_owner`, meterID).Scan(&owner); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		isOwner := owner == nil
		if !isOwner && ownership == "zieon" && *owner != p.UserID {
			return errf(http.StatusConflict, "meter_owned", "this meter belongs to another customer; ask your administrator to transfer it")
		}
		_, err = tx.Exec(ctx, `INSERT INTO meter_links (tenant_id, meter_id, user_id, label, is_owner) VALUES ($1,$2,$3,$4,$5)`,
			p.TenantID, meterID, p.UserID, in.Label, isOwner)
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "already_linked", "you have already added this meter")
		}
		if err != nil {
			return err
		}
		if isOwner && in.PropertyID != nil {
			if _, err := tx.Exec(ctx, `UPDATE meters SET property_id=$2 WHERE id=$1`, meterID, in.PropertyID); err != nil {
				return err
			}
		}
		out, err = myMeter(ctx, tx, p, meterID)
		return err
	})
	return out, err
}

func (s *Service) UnlinkMeter(ctx context.Context, p auth.Principal, meterID uuid.UUID) error {
	return s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM meter_links WHERE tenant_id=$1 AND user_id=$2 AND meter_id=$3`, p.TenantID, p.UserID, meterID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

type MeterSettingsInput struct {
	Label         *string    `json:"label"`
	LowBalanceKwh *float64   `json:"low_balance_threshold_kwh"`
	AutoTopup     *AutoTopup `json:"auto_topup"`
	PropertyID    *uuid.UUID `json:"property_id"`
}

func (s *Service) UpdateMeterSettings(ctx context.Context, p auth.Principal, t Tenant, meterID uuid.UUID, in MeterSettingsInput) (Meter, error) {
	if in.Label != nil && (strings.TrimSpace(*in.Label) == "" || len(*in.Label) > 40) {
		return Meter{}, errf(http.StatusBadRequest, "invalid_label", "label is required (max 40 chars)")
	}
	if in.LowBalanceKwh != nil && (*in.LowBalanceKwh < 0 || *in.LowBalanceKwh > 10000) {
		return Meter{}, errf(http.StatusBadRequest, "invalid_threshold", "low balance threshold must be 0-10000 kWh")
	}
	var out Meter
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		m, err := myMeter(ctx, tx, p, meterID)
		if err != nil {
			return err
		}
		var low *int64
		if in.LowBalanceKwh != nil {
			low = ptr(KwhToWh(*in.LowBalanceKwh))
		}
		var autoOn *bool
		var autoWh, autoAmt *int64
		if in.AutoTopup != nil {
			a := in.AutoTopup
			if a.Enabled {
				if !t.Enabled("auto_topup") || !t.Enabled("wallet") {
					return ErrFeatureOff
				}
				if a.AmountKobo <= 0 || a.ThresholdKwh <= 0 {
					return errf(http.StatusBadRequest, "invalid_auto_topup", "auto top-up needs a positive amount_kobo and threshold_kwh")
				}
				if !m.Capabilities.Telemetry {
					return errf(http.StatusConflict, "meter_capability", "auto top-up needs a meter that reports its balance (telemetry)")
				}
			}
			autoOn, autoWh, autoAmt = &a.Enabled, ptr(KwhToWh(a.ThresholdKwh)), &a.AmountKobo
		}
		if _, err := tx.Exec(ctx, `UPDATE meter_links SET label=coalesce($3,label), low_balance_threshold_wh=coalesce($4,low_balance_threshold_wh),
			auto_topup_enabled=coalesce($5,auto_topup_enabled), auto_topup_threshold_wh=coalesce($6,auto_topup_threshold_wh),
			auto_topup_amount_kobo=coalesce($7,auto_topup_amount_kobo) WHERE meter_id=$1 AND user_id=$2`,
			meterID, p.UserID, in.Label, low, autoOn, autoWh, autoAmt); err != nil {
			return err
		}
		if in.PropertyID != nil {
			if !m.IsOwner {
				return ErrForbidden
			}
			if err := ownsProperty(ctx, tx, p, *in.PropertyID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE meters SET property_id=$2, updated_at=now() WHERE id=$1`, meterID, in.PropertyID); err != nil {
				return err
			}
		}
		out, err = myMeter(ctx, tx, p, meterID)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Readings and consumption
// ---------------------------------------------------------------------------

type Consumption struct {
	MeterID    uuid.UUID          `json:"meter_id"`
	DataSource string             `json:"data_source"` // device | none
	Days       []DailyConsumption `json:"days"`
	TotalKwh   float64            `json:"total_kwh"`
	AvgKwh     float64            `json:"average_daily_kwh"`
	Breakdown  map[string]float64 `json:"time_breakdown"`
	Message    string             `json:"message,omitempty"`
}

type DailyConsumption struct {
	Date string  `json:"date"`
	Kwh  float64 `json:"kwh"`
}

// Consumption derives usage from cumulative register readings. Meters that do
// not report telemetry return data_source "none" rather than invented numbers.
func (s *Service) Consumption(ctx context.Context, p auth.Principal, meterID uuid.UUID, days int) (Consumption, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	c := Consumption{MeterID: meterID, DataSource: "none", Days: []DailyConsumption{}, Breakdown: map[string]float64{}}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		if _, err := myMeter(ctx, tx, p, meterID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			WITH r AS (
				SELECT recorded_at, cumulative_wh - lag(cumulative_wh) OVER (ORDER BY recorded_at) AS used
				FROM meter_readings WHERE meter_id=$1 AND cumulative_wh IS NOT NULL AND recorded_at > now() - make_interval(days => $2 + 1)
			)
			SELECT to_char(date_trunc('day', recorded_at), 'YYYY-MM-DD') d,
				coalesce(sum(used) FILTER (WHERE used > 0), 0),
				coalesce(sum(used) FILTER (WHERE used > 0 AND extract(hour FROM recorded_at) BETWEEN 6 AND 11), 0),
				coalesce(sum(used) FILTER (WHERE used > 0 AND extract(hour FROM recorded_at) BETWEEN 12 AND 17), 0),
				coalesce(sum(used) FILTER (WHERE used > 0 AND (extract(hour FROM recorded_at) >= 18 OR extract(hour FROM recorded_at) < 6)), 0)
			FROM r WHERE used IS NOT NULL AND recorded_at > now() - make_interval(days => $2)
			GROUP BY 1 ORDER BY 1`, meterID, days)
		if err != nil {
			return err
		}
		defer rows.Close()
		var morning, afternoon, night, total int64
		for rows.Next() {
			var d string
			var used, mo, af, ni int64
			if err := rows.Scan(&d, &used, &mo, &af, &ni); err != nil {
				return err
			}
			c.Days = append(c.Days, DailyConsumption{Date: d, Kwh: WhToKwh(used)})
			total += used
			morning, afternoon, night = morning+mo, afternoon+af, night+ni
		}
		if len(c.Days) > 0 {
			c.DataSource = "device"
			c.TotalKwh = WhToKwh(total)
			c.AvgKwh = c.TotalKwh / float64(len(c.Days))
			if total > 0 {
				c.Breakdown = map[string]float64{"morning": float64(morning) / float64(total),
					"afternoon": float64(afternoon) / float64(total), "night": float64(night) / float64(total)}
			}
		} else {
			c.Message = "This meter has not reported consumption data."
		}
		return rows.Err()
	})
	return c, err
}

type Reading struct {
	RecordedAt    time.Time `json:"recorded_at"`
	CreditKwh     *float64  `json:"credit_kwh"`
	CumulativeKwh *float64  `json:"cumulative_kwh"`
	VoltageV      *float64  `json:"voltage_v"`
	PowerStatus   *string   `json:"power_status"`
	Tamper        bool      `json:"tamper"`
}

func (s *Service) Readings(ctx context.Context, p auth.Principal, meterID uuid.UUID, limit int) ([]Reading, error) {
	out := []Reading{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		if p.Kind == "user" && p.Role == auth.RoleCustomer {
			if _, err := myMeter(ctx, tx, p, meterID); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT recorded_at, credit_wh, cumulative_wh, voltage_dv, power_status, tamper FROM meter_readings
			WHERE meter_id=$1 AND tenant_id=$2 ORDER BY recorded_at DESC LIMIT $3`, meterID, p.TenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Reading
			var credit, cum *int64
			var dv *int
			if err := rows.Scan(&r.RecordedAt, &credit, &cum, &dv, &r.PowerStatus, &r.Tamper); err != nil {
				return err
			}
			if credit != nil {
				r.CreditKwh = ptr(WhToKwh(*credit))
			}
			if cum != nil {
				r.CumulativeKwh = ptr(WhToKwh(*cum))
			}
			if dv != nil {
				r.VoltageV = ptr(float64(*dv) / 10)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Ownership transfer (customer requests, tenant administrator decides)
// ---------------------------------------------------------------------------

type Transfer struct {
	ID         uuid.UUID  `json:"id"`
	MeterID    uuid.UUID  `json:"meter_id"`
	FromUserID uuid.UUID  `json:"from_user_id"`
	ToUserID   uuid.UUID  `json:"to_user_id"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason"`
	DecidedAt  *time.Time `json:"decided_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

const transferCols = `id, meter_id, from_user_id, to_user_id, status, reason, decided_at, created_at`

func scanTransfer(row pgx.Row) (Transfer, error) {
	var t Transfer
	err := row.Scan(&t.ID, &t.MeterID, &t.FromUserID, &t.ToUserID, &t.Status, &t.Reason, &t.DecidedAt, &t.CreatedAt)
	return t, notFoundIfNoRows(err)
}

func (s *Service) RequestTransfer(ctx context.Context, p auth.Principal, meterID uuid.UUID, toEmail, reason string) (Transfer, error) {
	var out Transfer
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		m, err := myMeter(ctx, tx, p, meterID)
		if err != nil {
			return err
		}
		if !m.IsOwner {
			return errf(http.StatusForbidden, "not_owner", "only the meter owner can transfer it")
		}
		var to uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM users WHERE tenant_id=$1 AND lower(email)=lower($2) AND role='customer' AND status='active'`,
			p.TenantID, strings.TrimSpace(toEmail)).Scan(&to)
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(http.StatusNotFound, "recipient_not_found", "no active customer with that email in this organisation")
		}
		if err != nil {
			return err
		}
		if to == p.UserID {
			return errf(http.StatusBadRequest, "invalid_recipient", "you already own this meter")
		}
		out, err = scanTransfer(tx.QueryRow(ctx, `INSERT INTO meter_transfers (tenant_id, meter_id, from_user_id, to_user_id, reason)
			VALUES ($1,$2,$3,$4,$5) RETURNING `+transferCols, p.TenantID, meterID, p.UserID, to, reason))
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "transfer_pending", "a transfer for this meter is already pending")
		}
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &p.TenantID, Action: "meter.transfer.request", TargetType: "meter", TargetID: meterID.String(),
			Metadata: map[string]any{"to_user_id": to}})
	})
	return out, err
}

func (s *Service) ListTransfers(ctx context.Context, tenant uuid.UUID, status string) ([]Transfer, error) {
	out := []Transfer{}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+transferCols+` FROM meter_transfers WHERE tenant_id=$1 AND ($2='' OR status=$2) ORDER BY created_at DESC LIMIT 200`, tenant, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTransfer(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) DecideTransfer(ctx context.Context, p auth.Principal, transferID uuid.UUID, approve bool) (Transfer, error) {
	var out Transfer
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		t, err := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferCols+` FROM meter_transfers WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, transferID, p.TenantID))
		if err != nil {
			return err
		}
		if t.Status != "pending" {
			return errf(http.StatusConflict, "transfer_closed", "this transfer is already %s", t.Status)
		}
		status := "rejected"
		if approve {
			status = "approved"
			if _, err := tx.Exec(ctx, `DELETE FROM meter_links WHERE meter_id=$1 AND user_id=$2`, t.MeterID, t.FromUserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO meter_links (tenant_id, meter_id, user_id, label, is_owner) VALUES ($1,$2,$3,'Transferred meter',true)
				ON CONFLICT (meter_id, user_id) DO UPDATE SET is_owner=true`, p.TenantID, t.MeterID, t.ToUserID); err != nil {
				return err
			}
			// Pending tokens issued to the previous owner for this meter stay valid but are re-homed.
			if _, err := tx.Exec(ctx, `UPDATE energy_tokens SET issued_to_user_id=$3 WHERE meter_id=$1 AND issued_to_user_id=$2 AND status='issued'`,
				t.MeterID, t.FromUserID, t.ToUserID); err != nil {
				return err
			}
			if err := notify(ctx, tx, p.TenantID, &t.ToUserID, "Meter transferred to you", "A meter has been added to your account.", "meter", false); err != nil {
				return err
			}
		}
		out, err = scanTransfer(tx.QueryRow(ctx, `UPDATE meter_transfers SET status=$2, decided_by=$3, decided_at=now() WHERE id=$1 RETURNING `+transferCols,
			transferID, status, p.UserID))
		if err != nil {
			return err
		}
		if err := notify(ctx, tx, p.TenantID, &t.FromUserID, "Meter transfer "+status, "Your meter transfer request was "+status+".", "meter", false); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &p.TenantID, Action: "meter.transfer." + status, TargetType: "meter", TargetID: t.MeterID.String(),
			Metadata: map[string]any{"transfer_id": transferID, "from": t.FromUserID, "to": t.ToUserID}})
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Tenant administration of meters
// ---------------------------------------------------------------------------

func (s *Service) TenantMeters(ctx context.Context, tenant uuid.UUID, status string, limit int) ([]Meter, error) {
	out := []Meter{}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, meterSelect+`WHERE m.tenant_id=$1 AND ($2::uuid IS NULL) AND ($3='' OR m.status=$3) ORDER BY m.created_at DESC LIMIT $4`,
			tenant, nil, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMeter(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

type AssignMeterInput struct {
	UserID       uuid.UUID  `json:"user_id"`
	Label        string     `json:"label"`
	PropertyID   *uuid.UUID `json:"property_id"`
	CustomerName string     `json:"customer_name"`
	Address      string     `json:"address"`
}

// AssignMeterToCustomer is the tenant-admin path for Zieon hardware: the meter
// must already be assigned to this tenant by the platform.
func (s *Service) AssignMeterToCustomer(ctx context.Context, tenant, meterID uuid.UUID, in AssignMeterInput) error {
	if in.Label == "" {
		in.Label = "Meter"
	}
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM meters WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, meterID, tenant).Scan(&status); err != nil {
			return notFoundIfNoRows(err)
		}
		if status == "decommissioned" {
			return errf(http.StatusConflict, "meter_unavailable", "meter is decommissioned")
		}
		var role string
		if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 AND tenant_id=$2`, in.UserID, tenant).Scan(&role); err != nil {
			return errf(http.StatusNotFound, "customer_not_found", "customer not found in this organisation")
		}
		if role != auth.RoleCustomer {
			return errf(http.StatusBadRequest, "invalid_customer", "meters can only be assigned to customers")
		}
		if in.PropertyID != nil {
			var owner *uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT owner_user_id FROM properties WHERE id=$1 AND tenant_id=$2`, *in.PropertyID, tenant).Scan(&owner); err != nil {
				return errf(http.StatusNotFound, "property_not_found", "property not found")
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO meter_links (tenant_id, meter_id, user_id, label, is_owner) VALUES ($1,$2,$3,$4,true)`, tenant, meterID, in.UserID, in.Label)
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "meter_owned", "meter already has an owner; use an ownership transfer")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE meters SET status='active', property_id=coalesce($2,property_id),
			customer_name=coalesce(nullif($3,''),customer_name), address=coalesce(nullif($4,''),address), updated_at=now() WHERE id=$1`,
			meterID, in.PropertyID, in.CustomerName, in.Address); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "meter.assign_customer", TargetType: "meter", TargetID: meterID.String(),
			Metadata: map[string]any{"user_id": in.UserID, "property_id": in.PropertyID}})
	})
}

func (s *Service) SetMeterStatus(ctx context.Context, tenant, meterID uuid.UUID, status string) error {
	if status != "active" && status != "inactive" {
		return errf(http.StatusBadRequest, "invalid_status", "status must be active or inactive")
	}
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE meters SET status=$3, updated_at=now() WHERE id=$1 AND tenant_id=$2 AND status IN ('assigned','active','inactive')`,
			meterID, tenant, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "meter.status", TargetType: "meter", TargetID: meterID.String(),
			Metadata: map[string]any{"status": status}})
	})
}

// ---------------------------------------------------------------------------
// Platform meter registry and device identity
// ---------------------------------------------------------------------------

type RegisterMeterInput struct {
	SerialNumber    string       `json:"serial_number"`
	MeterNumber     string       `json:"meter_number"`
	Model           string       `json:"model"`
	MeterType       string       `json:"meter_type"`
	CommsMode       string       `json:"comms_mode"`
	Capabilities    Capabilities `json:"capabilities"`
	FirmwareVersion string       `json:"firmware_version"`
}

type RegisteredMeter struct {
	ID           uuid.UUID           `json:"id"`
	SerialNumber string              `json:"serial_number"`
	MeterNumber  string              `json:"meter_number"`
	DeviceSecret string              `json:"device_secret,omitempty"` // shown once, for provisioning into firmware
	MQTT         *MQTTConnectionInfo `json:"mqtt,omitempty"`
}

var serialRe = regexp.MustCompile(`^[A-Z0-9-]{6,40}$`)

func (s *Service) RegisterMeter(ctx context.Context, in RegisterMeterInput) (RegisteredMeter, error) {
	in.SerialNumber = strings.ToUpper(strings.TrimSpace(in.SerialNumber))
	if !serialRe.MatchString(in.SerialNumber) {
		return RegisteredMeter{}, errf(http.StatusBadRequest, "invalid_serial", "serial_number must be 6-40 of A-Z, 0-9, -")
	}
	if !meterNumberRe.MatchString(in.MeterNumber) {
		return RegisteredMeter{}, errf(http.StatusBadRequest, "invalid_meter_number", "meter number must be 10-20 digits")
	}
	if in.MeterType == "" {
		in.MeterType = "prepaid"
	}
	if in.CommsMode == "" {
		in.CommsMode = "none"
	}
	switch in.CommsMode {
	case "none", "gsm", "wifi", "mqtt", "lorawan":
	default:
		return RegisteredMeter{}, errf(http.StatusBadRequest, "invalid_comms_mode", "comms_mode must be none, gsm, wifi, mqtt or lorawan")
	}
	if in.CommsMode == "none" && (in.Capabilities.Telemetry || in.Capabilities.RemoteCredit || in.Capabilities.RemoteDebit || in.Capabilities.Relay) {
		return RegisteredMeter{}, errf(http.StatusBadRequest, "invalid_capabilities", "a meter without communications cannot have remote capabilities")
	}
	out := RegisteredMeter{SerialNumber: in.SerialNumber, MeterNumber: in.MeterNumber}
	var secretEnc []byte
	if in.CommsMode != "none" {
		out.DeviceSecret = "zds_" + secure.RandomToken(32)
		var err error
		if secretEnc, err = s.Keys.Encrypt([]byte(out.DeviceSecret)); err != nil {
			return out, err
		}
	}
	caps, _ := json.Marshal(in.Capabilities)
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO meters (serial_number, meter_number, provider_code, ownership, meter_type, model, comms_mode, capabilities, firmware_version, device_secret_enc)
			VALUES ($1,$2,'ZIEON','zieon',$3,$4,$5,$6,$7,$8) RETURNING id`, in.SerialNumber, in.MeterNumber, in.MeterType, in.Model, in.CommsMode, caps,
			in.FirmwareVersion, secretEnc).Scan(&out.ID)
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "meter_exists", "a meter with that serial or meter number is already registered")
		}
		if err != nil {
			return err
		}
		if in.CommsMode == "mqtt" {
			out.MQTT = s.mqttInfo(in.SerialNumber)
		}
		return s.audit(ctx, tx, AuditEntry{Action: "platform.meter.register", TargetType: "meter", TargetID: out.ID.String(),
			Metadata: map[string]any{"serial": in.SerialNumber, "comms": in.CommsMode}})
	})
	return out, err
}

// AssignMeterToTenant moves inventory to a partner. Re-assigning a meter that
// has customers requires decommission/transfer first.
func (s *Service) AssignMeterToTenant(ctx context.Context, meterID, tenantID uuid.UUID) error {
	return s.DB.System(ctx, func(tx pgx.Tx) error {
		var links int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM meter_links WHERE meter_id=$1`, meterID).Scan(&links); err != nil {
			return err
		}
		if links > 0 {
			return errf(http.StatusConflict, "meter_in_use", "meter is linked to customers; transfer or unlink first")
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id=$1 AND status='active')`, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errf(http.StatusNotFound, "tenant_not_found", "tenant not found or suspended")
		}
		tag, err := tx.Exec(ctx, `UPDATE meters SET tenant_id=$2, status='assigned', property_id=NULL, updated_at=now()
			WHERE id=$1 AND ownership='zieon' AND status IN ('inventory','assigned','inactive')`, meterID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenantID, Action: "platform.meter.assign_tenant", TargetType: "meter", TargetID: meterID.String()})
	})
}

func (s *Service) PlatformMeters(ctx context.Context, status string, limit int) ([]Meter, error) {
	out := []Meter{}
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, meterSelect+`WHERE ($1::uuid IS NULL) AND ($2::uuid IS NULL) AND m.ownership='zieon' AND ($3='' OR m.status=$3)
			ORDER BY m.created_at DESC LIMIT $4`, nil, nil, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMeter(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

const deviceClockSkew = 5 * time.Minute

// AuthenticateDevice verifies a signed device request:
//
//	X-Device-Serial, X-Device-Timestamp (unix seconds), X-Device-Nonce,
//	X-Device-Signature = hex(HMAC-SHA256(device_secret, serial\ntimestamp\nnonce\nbody))
//
// The timestamp window plus a stored nonce prevent replay.
func (s *Service) AuthenticateDevice(ctx context.Context, serial, ts, nonce, sig string, body []byte) (auth.Principal, error) {
	invalid := errf(http.StatusUnauthorized, "invalid_device_signature", "device authentication failed")
	unix, err := parseUnix(ts)
	if err != nil || len(nonce) < 8 || len(nonce) > 64 {
		return auth.Principal{}, invalid
	}
	if d := s.Now().Sub(unix); d > deviceClockSkew || d < -deviceClockSkew {
		return auth.Principal{}, invalid
	}
	var p auth.Principal
	err = s.DB.System(ctx, func(tx pgx.Tx) error {
		var enc []byte
		var tenant *uuid.UUID
		var status string
		if err := tx.QueryRow(ctx, `SELECT id, tenant_id, device_secret_enc, status FROM meters WHERE serial_number=$1 AND ownership='zieon'`,
			strings.ToUpper(serial)).Scan(&p.MeterID, &tenant, &enc, &status); err != nil {
			return invalid
		}
		if enc == nil || tenant == nil || status == "decommissioned" {
			return invalid
		}
		secret, err := s.Keys.Decrypt(enc)
		if err != nil {
			return err
		}
		expected := secure.HMACSHA256Hex(secret, strings.ToUpper(serial)+"\n"+ts+"\n"+nonce+"\n"+string(body))
		if !secure.EqualHex(sig, expected) {
			return invalid
		}
		tag, err := tx.Exec(ctx, `INSERT INTO device_nonces (meter_id, nonce) VALUES ($1,$2) ON CONFLICT DO NOTHING`, p.MeterID, nonce)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid // replay
		}
		p.Kind, p.TenantID = "device", *tenant
		return nil
	})
	var appErr *Error
	if errors.As(err, &appErr) {
		return auth.Principal{}, appErr
	}
	return p, err
}

type Telemetry struct {
	RecordedAt      *time.Time `json:"recorded_at"`
	CreditKwh       *float64   `json:"credit_kwh"`
	CumulativeKwh   *float64   `json:"cumulative_kwh"`
	VoltageV        *float64   `json:"voltage_v"`
	CurrentA        *float64   `json:"current_a"`
	PowerStatus     *string    `json:"power_status"`
	Tamper          bool       `json:"tamper"`
	FirmwareVersion string     `json:"firmware_version"`
	// AcceptedTokens lists tokens the meter has accepted since the last report;
	// this is the only signal that marks a live token "delivered".
	AcceptedTokens []string `json:"accepted_tokens"`
}

type TelemetryResult struct {
	Accepted        bool `json:"accepted"`
	TokensConfirmed int  `json:"tokens_confirmed"`
}

func (s *Service) IngestTelemetry(ctx context.Context, p auth.Principal, in Telemetry) (TelemetryResult, error) {
	res := TelemetryResult{Accepted: true}
	at := s.Now()
	if in.RecordedAt != nil {
		if in.RecordedAt.After(at.Add(deviceClockSkew)) || in.RecordedAt.Before(at.Add(-72*time.Hour)) {
			return res, errf(http.StatusBadRequest, "invalid_timestamp", "recorded_at is out of range")
		}
		at = *in.RecordedAt
	}
	if in.PowerStatus != nil {
		switch *in.PowerStatus {
		case "lightOn", "lowCurrent", "noLight":
		default:
			return res, errf(http.StatusBadRequest, "invalid_power_status", "power_status must be lightOn, lowCurrent or noLight")
		}
	}
	if len(in.AcceptedTokens) > 20 {
		return res, errf(http.StatusBadRequest, "too_many_tokens", "at most 20 accepted tokens per report")
	}
	var creditWh, cumWh *int64
	if in.CreditKwh != nil {
		creditWh = ptr(KwhToWh(*in.CreditKwh))
	}
	if in.CumulativeKwh != nil {
		cumWh = ptr(KwhToWh(*in.CumulativeKwh))
	}
	var dv, ma *int
	if in.VoltageV != nil {
		dv = ptr(int(*in.VoltageV * 10))
	}
	if in.CurrentA != nil {
		ma = ptr(int(*in.CurrentA * 1000))
	}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO meter_readings (tenant_id, meter_id, recorded_at, credit_wh, cumulative_wh, voltage_dv, current_ma, power_status, tamper)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.TenantID, p.MeterID, at, creditWh, cumWh, dv, ma, in.PowerStatus, in.Tamper); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE meters SET last_seen_at=now(), connectivity='online',
			firmware_version=coalesce(nullif($2,''), firmware_version), updated_at=now() WHERE id=$1`, p.MeterID, in.FirmwareVersion); err != nil {
			return err
		}
		if in.Tamper {
			var open bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM meter_alerts WHERE meter_id=$1 AND kind='tamper' AND resolved_at IS NULL)`, p.MeterID).Scan(&open); err != nil {
				return err
			}
			if !open {
				if _, err := tx.Exec(ctx, `INSERT INTO meter_alerts (tenant_id, meter_id, kind, message) VALUES ($1,$2,'tamper','Meter reported a tamper condition')`,
					p.TenantID, p.MeterID); err != nil {
					return err
				}
				if err := emit(ctx, tx, p.TenantID, "meter.tamper", map[string]any{"meter_id": p.MeterID}); err != nil {
					return err
				}
			}
		}
		for _, tok := range in.AcceptedTokens {
			n, err := s.confirmTokenDelivery(ctx, tx, p.TenantID, p.MeterID, normalizeToken(tok))
			if err != nil {
				return err
			}
			res.TokensConfirmed += n
		}
		if creditWh != nil {
			return s.checkLowBalance(ctx, tx, p.TenantID, p.MeterID, *creditWh)
		}
		return nil
	})
	return res, err
}

// checkLowBalance notifies linked customers once per dip below their threshold
// and triggers auto top-up where enabled.
func (s *Service) checkLowBalance(ctx context.Context, tx pgx.Tx, tenant, meterID uuid.UUID, creditWh int64) error {
	rows, err := tx.Query(ctx, `SELECT id, user_id, label, low_balance_threshold_wh, low_balance_notified_at IS NOT NULL,
		auto_topup_enabled, auto_topup_threshold_wh, auto_topup_amount_kobo FROM meter_links WHERE meter_id=$1`, meterID)
	if err != nil {
		return err
	}
	type link struct {
		id, user                  uuid.UUID
		label                     string
		threshold                 int64
		notified, auto            bool
		autoThreshold, autoAmount int64
	}
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.id, &l.user, &l.label, &l.threshold, &l.notified, &l.auto, &l.autoThreshold, &l.autoAmount); err != nil {
			rows.Close()
			return err
		}
		links = append(links, l)
	}
	rows.Close()
	for _, l := range links {
		switch {
		case creditWh < l.threshold && !l.notified:
			if _, err := tx.Exec(ctx, `UPDATE meter_links SET low_balance_notified_at=now() WHERE id=$1`, l.id); err != nil {
				return err
			}
			msg := l.label + " meter is running low on units."
			if err := notify(ctx, tx, tenant, &l.user, "Low balance", msg, "low_balance", true); err != nil {
				return err
			}
			if err := emit(ctx, tx, tenant, "meter.low_balance", map[string]any{"meter_id": meterID, "user_id": l.user, "credit_kwh": WhToKwh(creditWh)}); err != nil {
				return err
			}
		case creditWh >= l.threshold && l.notified:
			if _, err := tx.Exec(ctx, `UPDATE meter_links SET low_balance_notified_at=NULL WHERE id=$1`, l.id); err != nil {
				return err
			}
		}
		if l.auto && creditWh < l.autoThreshold && l.autoAmount > 0 {
			// Queued: the worker runs the purchase outside this telemetry transaction.
			if _, err := tx.Exec(ctx, `INSERT INTO auto_topup_jobs (tenant_id, link_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				tenant, l.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseUnix(s string) (time.Time, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return time.Time{}, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	if n == 0 {
		return time.Time{}, errors.New("empty")
	}
	return time.Unix(n, 0), nil
}

// ---------------------------------------------------------------------------
// Properties (portfolio management)
// ---------------------------------------------------------------------------

type Property struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Address     string     `json:"address"`
	OwnerUserID *uuid.UUID `json:"owner_user_id"`
	Meters      []Meter    `json:"meters"`
	TotalKwh    float64    `json:"total_kwh_reserve"`
	CreatedAt   time.Time  `json:"created_at"`
}

func ownsProperty(ctx context.Context, tx pgx.Tx, p auth.Principal, id uuid.UUID) error {
	var owner *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT owner_user_id FROM properties WHERE id=$1 AND tenant_id=$2`, id, p.TenantID).Scan(&owner)
	if err != nil || owner == nil || *owner != p.UserID {
		return errf(http.StatusNotFound, "property_not_found", "property not found")
	}
	return nil
}

func (s *Service) CreateProperty(ctx context.Context, tenant uuid.UUID, owner *uuid.UUID, name, address string) (Property, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || len(address) > 200 {
		return Property{}, errf(http.StatusBadRequest, "invalid_input", "name is required (max 80), address max 200")
	}
	p := Property{Name: name, Address: address, OwnerUserID: owner, Meters: []Meter{}}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		if owner != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id=$1 AND tenant_id=$2 AND role='customer')`, owner, tenant).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return errf(http.StatusNotFound, "customer_not_found", "owner must be a customer in this organisation")
			}
		}
		return tx.QueryRow(ctx, `INSERT INTO properties (tenant_id, owner_user_id, name, address) VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
			tenant, owner, name, address).Scan(&p.ID, &p.CreatedAt)
	})
	return p, err
}

// Properties lists a customer's portfolio (or every property for tenant operators when userID is nil).
func (s *Service) Properties(ctx context.Context, p auth.Principal, userID *uuid.UUID) ([]Property, error) {
	out := []Property{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, name, address, owner_user_id, created_at FROM properties
			WHERE tenant_id=$1 AND ($2::uuid IS NULL OR owner_user_id=$2) ORDER BY created_at`, p.TenantID, userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var pr Property
			if err := rows.Scan(&pr.ID, &pr.Name, &pr.Address, &pr.OwnerUserID, &pr.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			pr.Meters = []Meter{}
			out = append(out, pr)
		}
		rows.Close()
		for i := range out {
			mrows, err := tx.Query(ctx, meterSelect+`WHERE m.tenant_id=$1 AND m.property_id=$3 ORDER BY m.created_at`, p.TenantID, userID, out[i].ID)
			if err != nil {
				return err
			}
			for mrows.Next() {
				m, err := scanMeter(mrows)
				if err != nil {
					mrows.Close()
					return err
				}
				out[i].Meters = append(out[i].Meters, m)
				out[i].TotalKwh += m.KwhReserve
			}
			mrows.Close()
		}
		return nil
	})
	return out, err
}
