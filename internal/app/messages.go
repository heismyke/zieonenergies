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

	"github.com/heismyke/zieonenergies/internal/messaging"
)

// DeliverTo is where a customer wants their token sent besides the app.
type DeliverTo struct {
	Email    bool   `json:"email"`
	SMS      bool   `json:"sms"`
	WhatsApp bool   `json:"whatsapp"`
	Phone    string `json:"phone"` // for SMS/WhatsApp; defaults to the profile phone
}

// DeliveryChannels lists the channels a buyer can choose, in display order.
func (s *Service) DeliveryChannels() []string {
	out := []string{}
	for _, c := range []string{messaging.Email, messaging.SMS, messaging.WhatsApp} {
		if s.Messengers[c] != nil {
			out = append(out, c)
		}
	}
	return out
}

// queueTokenMessages records the channels to send an order's token to. It runs
// inside the order's transaction; the messages go out once the token exists.
func queueTokenMessages(ctx context.Context, tx pgx.Tx, tenant, user, vendID uuid.UUID, d DeliverTo) error {
	if !d.Email && !d.SMS && !d.WhatsApp {
		return nil
	}
	var email, profilePhone string
	if err := tx.QueryRow(ctx, `SELECT email, phone FROM users WHERE id=$1`, user).Scan(&email, &profilePhone); err != nil {
		return err
	}
	phone := ""
	if d.SMS || d.WhatsApp {
		raw := d.Phone
		if strings.TrimSpace(raw) == "" {
			raw = profilePhone
		}
		if phone = messaging.NormalizePhone(raw); phone == "" {
			return errf(http.StatusBadRequest, "invalid_phone", "enter a valid Nigerian phone number to get your token by SMS or WhatsApp")
		}
		// Remember the number for next time.
		if profilePhone == "" {
			if _, err := tx.Exec(ctx, `UPDATE users SET phone=$2 WHERE id=$1 AND phone=''`, user, strings.TrimSpace(d.Phone)); err != nil {
				return err
			}
		}
	}
	add := func(on bool, channel, to string) error {
		if !on {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbound_messages (tenant_id, vend_transaction_id, channel, destination) VALUES ($1,$2,$3,$4)
			ON CONFLICT (vend_transaction_id, channel) DO NOTHING`, tenant, vendID, channel, to)
		return err
	}
	if err := add(d.Email, messaging.Email, email); err != nil {
		return err
	}
	if err := add(d.SMS, messaging.SMS, phone); err != nil {
		return err
	}
	return add(d.WhatsApp, messaging.WhatsApp, phone)
}

type tokenMessage struct {
	id, tenant, vend      uuid.UUID
	channel, to, status   string
	attempts              int
	amount, fee, energyWh int64
	meter, disco, brand   string
	tokenEnc              []byte
	customer, method      string
	createdAt             time.Time
	logo, accent, support string
	domain                string
	sandbox               bool
}

// DeliverTokenMessages sends queued token receipts whose order has finished.
// Orders that failed are skipped; a failed send is retried with backoff.
func (s *Service) DeliverTokenMessages(ctx context.Context) error {
	var due []tokenMessage
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o.id, o.tenant_id, o.vend_transaction_id, o.channel, o.destination, o.attempts,
				v.status, v.amount_kobo, v.fee_kobo, v.energy_wh, m.meter_number,
				coalesce(t.provider_name, m.provider_code), coalesce(b.display_name, ''), et.token_enc,
				m.customer_name, v.payment_method, v.created_at, coalesce(b.logo_url, ''), coalesce(b.theme->>'primary', ''),
				coalesce(b.support_email, ''), coalesce(b.custom_domain, ''), v.sandbox
			FROM outbound_messages o
			JOIN vend_transactions v ON v.id = o.vend_transaction_id
			JOIN meters m ON m.id = v.meter_id
			LEFT JOIN tariffs t ON t.tenant_id = o.tenant_id AND t.provider_code = m.provider_code
			LEFT JOIN tenant_branding b ON b.tenant_id = o.tenant_id
			LEFT JOIN energy_tokens et ON et.vend_transaction_id = v.id
			WHERE o.status = 'pending' AND o.next_attempt_at <= now()
				AND v.status IN ('vended','delivered','payment_failed','vend_failed','refunded')
			ORDER BY o.created_at LIMIT 25 FOR UPDATE OF o SKIP LOCKED`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m tokenMessage
			if err := rows.Scan(&m.id, &m.tenant, &m.vend, &m.channel, &m.to, &m.attempts, &m.status, &m.amount, &m.fee,
				&m.energyWh, &m.meter, &m.disco, &m.brand, &m.tokenEnc,
				&m.customer, &m.method, &m.createdAt, &m.logo, &m.accent, &m.support, &m.domain, &m.sandbox); err != nil {
				rows.Close()
				return err
			}
			due = append(due, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Claim the batch so another instance does not send it too.
		for _, m := range due {
			if _, err := tx.Exec(ctx, `UPDATE outbound_messages SET attempts = attempts + 1, next_attempt_at = now() + interval '5 minutes' WHERE id=$1`, m.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range due {
		s.sendTokenMessage(ctx, m)
	}
	return nil
}

func (s *Service) sendTokenMessage(ctx context.Context, m tokenMessage) {
	finish := func(status, ref, errText string, retryIn time.Duration) {
		err := s.DB.Tenant(ctx, m.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE outbound_messages SET status=$2, provider_ref=$3, last_error=$4,
				sent_at = CASE WHEN $2 = 'sent' THEN now() ELSE sent_at END,
				next_attempt_at = now() + $5::interval WHERE id=$1`, m.id, status, ref, errText, fmt.Sprintf("%d seconds", int(retryIn.Seconds())))
			return err
		})
		if err != nil {
			s.Log.Error("token message: update", "id", m.id, "err", err)
		}
	}
	if m.status != StVended && m.status != StDelivered {
		finish("skipped", "", "order did not complete", 0)
		return
	}
	sender := s.Messengers[m.channel]
	if sender == nil {
		finish("skipped", "", m.channel+" is not configured", 0)
		return
	}
	token := ""
	if m.tokenEnc != nil {
		plain, err := s.Keys.Decrypt(m.tokenEnc)
		if err != nil {
			s.Log.Error("token message: decrypt", "id", m.id, "err", err)
			finish("failed", "", "could not read token", 0)
			return
		}
		token = string(plain)
	}
	ref, err := sender.Send(ctx, renderTokenMessage(m, token, s.siteURL(m)))
	switch {
	case err == nil:
		finish("sent", ref, "", 0)
	case errors.Is(err, messaging.ErrPermanent) || m.attempts+1 >= 5:
		s.Log.Warn("token message failed", "id", m.id, "channel", m.channel, "err", err)
		finish("failed", "", err.Error(), 0)
	default:
		// Back off: 1, 2, 4, 8 minutes.
		finish("pending", "", err.Error(), time.Duration(1<<m.attempts)*time.Minute)
	}
}

// groupToken formats a 20-digit token as 1234-5678-9012-3456-7890.
func groupToken(t string) string {
	var b strings.Builder
	for i, r := range t {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// siteURL is the tenant's buy site, for links in messages.
func (s *Service) siteURL(m tokenMessage) string { return s.siteFor(m.domain) }

func renderTokenMessage(m tokenMessage, token, site string) messaging.Message {
	brand := m.brand
	if brand == "" {
		brand = "Zenerlink"
	}
	ref := strings.ToUpper(m.vend.String()[:8])
	naira := "₦" + nairaStr(m.amount-m.fee)
	kwh := fmt.Sprintf("%.1f kWh", WhToKwh(m.energyWh))
	tok := groupToken(token)
	if m.channel != messaging.Email {
		if token == "" {
			return messaging.Message{To: m.to, Text: fmt.Sprintf("%s: Your %s bill payment of N%s for meter %s was received. Ref %s", brand, m.disco, nairaStr(m.amount-m.fee), m.meter, ref)}
		}
		// "N" not "₦": one non-GSM character makes an SMS cost twice as much.
		return messaging.Message{To: m.to, Text: fmt.Sprintf("%s: Your %s token for meter %s is %s (%s, N%s). Ref %s",
			brand, m.disco, m.meter, tok, kwh, nairaStr(m.amount-m.fee), ref)}
	}

	e := tokenEmail{Brand: strings.ToUpper(brand), BrandName: brand, Sandbox: m.sandbox, Logo: m.logo, Accent: m.accent, Support: m.support, SiteURL: site}
	if e.Logo == "" && site != "" && brand == "Zenerlink" {
		e.Logo = site + "/icon-192.png"
	}
	if site != "" {
		e.ReceiptURL = site + "/order/" + m.vend.String()
		e.TermsURL, e.PrivacyURL = site+"/terms", site+"/privacy"
	}
	e.Paid = "Card"
	if m.method == "wallet" {
		e.Paid = "Wallet"
	}
	e.Meter, e.Disco, e.Name, e.Ref = m.meter, m.disco, m.customer, ref
	e.Date = m.createdAt.In(lagos).Format("2 Jan 2006, 3:04 PM")
	e.Kwh = fmt.Sprintf("%.1f", WhToKwh(m.energyWh))
	e.Electricity = naira
	if m.fee > 0 {
		e.Fee = "₦" + nairaStr(m.fee)
	}
	e.Total = "₦" + nairaStr(m.amount)

	var subject, text string
	if token == "" {
		// Postpaid: the bill is paid, there is no token.
		e.Headline = fmt.Sprintf("%s bill paid", naira)
		e.Preheader = fmt.Sprintf("Your %s bill payment for meter %s was received.", m.disco, m.meter)
		subject = fmt.Sprintf("%s bill paid: %s", m.disco, naira)
		text = fmt.Sprintf("Your %s bill payment of %s for meter %s was received.\n\nReference: %s\n\n%s", m.disco, naira, m.meter, ref, brand)
	} else {
		e.Token = tok
		e.Headline = fmt.Sprintf("%s loaded: %s", kwh, m.disco)
		e.Preheader = fmt.Sprintf("Token %s · %s for meter %s", tok, kwh, m.meter)
		subject = fmt.Sprintf("Your %s token: %s", m.disco, tok)
		text = fmt.Sprintf("Your %s token\n\n%s\n\nMeter: %s\nUnits: %s\nAmount: %s\nTotal paid: %s\nReference: %s\n\nType the 20 digits on your meter keypad and press Enter.\n\n%s",
			m.disco, tok, m.meter, kwh, naira, e.Total, ref, brand)
	}
	if e.ReceiptURL != "" {
		text += "\nReceipt: " + e.ReceiptURL
	}
	return messaging.Message{To: m.to, Subject: subject, Text: text, HTML: renderTokenEmail(e)}
}
