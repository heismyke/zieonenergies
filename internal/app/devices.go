package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/secure"
)

// MQTT device protocol (see docs/MQTT_DEVICE_PROTOCOL.md).
//
//	connect:  client id = username = SERIAL
//	          password  = "v1:<unix>:<hex HMAC-SHA256(secret, "mqtt-connect\nSERIAL\n<unix>")>"
//	uplink:   zieon/v1/devices/SERIAL/{telemetry,acks,status}
//	downlink: zieon/v1/devices/SERIAL/commands
//
// Every uplink and downlink payload is an Envelope signed with the device
// secret, so a compromised or misconfigured broker cannot forge meter data
// or credit commands.

const TopicPrefix = "zieon/v1/devices/"

func DeviceTopic(serial, kind string) string { return TopicPrefix + serial + "/" + kind }

// Envelope wraps every MQTT payload. Data is the JSON document as a string so
// the signer and verifier hash exactly the same bytes.
type Envelope struct {
	TS    string          `json:"ts"`
	Nonce string          `json:"nonce"`
	Data  json.RawMessage `json:"data"`
	Sig   string          `json:"sig"`
}

// dataBytes accepts data as a JSON string (preferred) or an inline object.
func (e Envelope) dataBytes() ([]byte, error) {
	if len(e.Data) > 0 && e.Data[0] == '"' {
		var s string
		if err := json.Unmarshal(e.Data, &s); err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
	return e.Data, nil
}

var errBadDevice = errf(http.StatusUnauthorized, "invalid_device_credentials", "device authentication failed")

// AuthenticateMQTT verifies a CONNECT. The password is a time-bound HMAC, so
// the device secret itself never crosses the network, even without TLS.
func (s *Service) AuthenticateMQTT(ctx context.Context, clientID, username, password string) (auth.Principal, error) {
	serial := strings.ToUpper(username)
	if clientID == "" || !strings.EqualFold(clientID, username) || !serialRe.MatchString(serial) {
		return auth.Principal{}, errBadDevice
	}
	parts := strings.Split(password, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return auth.Principal{}, errBadDevice
	}
	ts, err := parseUnix(parts[1])
	if err != nil {
		return auth.Principal{}, errBadDevice
	}
	if d := s.Now().Sub(ts); d > deviceClockSkew || d < -deviceClockSkew {
		return auth.Principal{}, errBadDevice
	}
	var p auth.Principal
	err = s.DB.System(ctx, func(tx pgx.Tx) error {
		var enc []byte
		var tenant *uuid.UUID
		var status string
		if err := tx.QueryRow(ctx, `SELECT id, tenant_id, device_secret_enc, status FROM meters WHERE serial_number=$1 AND ownership='zieon'`,
			serial).Scan(&p.MeterID, &tenant, &enc, &status); err != nil {
			return errBadDevice
		}
		if enc == nil || tenant == nil || status == "decommissioned" {
			return errBadDevice
		}
		secret, err := s.Keys.Decrypt(enc)
		if err != nil {
			return err
		}
		if !secure.EqualHex(parts[2], secure.HMACSHA256Hex(secret, "mqtt-connect\n"+serial+"\n"+parts[1])) {
			return errBadDevice
		}
		p.Kind, p.TenantID = "device", *tenant
		return nil
	})
	return p, err
}

// DeviceMessage is a verified uplink ready for processing.
type DeviceMessage struct {
	Principal auth.Principal
	Data      []byte
}

// VerifyEnvelope checks an uplink envelope with the same signature, clock and
// nonce rules as signed HTTP telemetry: HMAC(secret, SERIAL\nts\nnonce\ndata).
func (s *Service) VerifyEnvelope(ctx context.Context, serial string, payload []byte) (DeviceMessage, error) {
	var e Envelope
	if err := json.Unmarshal(payload, &e); err != nil {
		return DeviceMessage{}, errf(http.StatusBadRequest, "invalid_envelope", "payload must be a signed envelope")
	}
	data, err := e.dataBytes()
	if err != nil {
		return DeviceMessage{}, errf(http.StatusBadRequest, "invalid_envelope", "data must be a JSON string or object")
	}
	p, err := s.AuthenticateDevice(ctx, serial, e.TS, e.Nonce, e.Sig, data)
	if err != nil {
		return DeviceMessage{}, err
	}
	return DeviceMessage{Principal: p, Data: data}, nil
}

// SetDeviceConnectivity records broker connect/disconnect.
func (s *Service) SetDeviceConnectivity(ctx context.Context, p auth.Principal, online bool) error {
	return s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		state := "offline"
		if online {
			state = "online"
		}
		tag, err := tx.Exec(ctx, `UPDATE meters SET connectivity=$2, last_seen_at=CASE WHEN $2='online' THEN now() ELSE last_seen_at END,
			updated_at=now() WHERE id=$1 AND connectivity <> $2`, p.MeterID, state)
		if err != nil || online || tag.RowsAffected() == 0 {
			return err
		}
		return emit(ctx, tx, p.TenantID, "meter.offline", map[string]any{"meter_id": p.MeterID, "reason": "mqtt_disconnect"})
	})
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

type Command struct {
	ID        uuid.UUID       `json:"id"`
	MeterID   uuid.UUID       `json:"meter_id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Status    string          `json:"status"`
	Attempts  int             `json:"attempts"`
	Result    string          `json:"result"`
	ExpiresAt time.Time       `json:"expires_at"`
	SentAt    *time.Time      `json:"sent_at"`
	AckedAt   *time.Time      `json:"acked_at"`
	CreatedAt time.Time       `json:"created_at"`
}

const commandCols = `id, meter_id, type, params, status, attempts, result, expires_at, sent_at, acked_at, created_at`

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	err := row.Scan(&c.ID, &c.MeterID, &c.Type, &c.Params, &c.Status, &c.Attempts, &c.Result, &c.ExpiresAt, &c.SentAt, &c.AckedAt, &c.CreatedAt)
	return c, notFoundIfNoRows(err)
}

var commandTTL = map[string]time.Duration{
	"credit_token": 24 * time.Hour, "relay": 10 * time.Minute, "sync": 5 * time.Minute, "ping": 5 * time.Minute,
}

func enqueueCommand(ctx context.Context, tx pgx.Tx, tenant, meter uuid.UUID, typ string, params any, createdBy *uuid.UUID) (Command, error) {
	ttl, ok := commandTTL[typ]
	if !ok {
		return Command{}, errf(http.StatusBadRequest, "invalid_command", "unknown command type %q", typ)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return Command{}, err
	}
	return scanCommand(tx.QueryRow(ctx, `INSERT INTO device_commands (tenant_id, meter_id, type, params, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+commandCols, tenant, meter, typ, raw, createdBy, time.Now().Add(ttl)))
}

type CommandInput struct {
	Type  string `json:"type"`  // relay | sync | ping
	State string `json:"state"` // relay: on | off
}

// SendMeterCommand is the operator path (tenant admins). Token credits are
// only ever queued by RedeemToken, never directly.
func (s *Service) SendMeterCommand(ctx context.Context, tenant, meterID uuid.UUID, in CommandInput) (Command, error) {
	var out Command
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var comms string
		var capsRaw []byte
		if err := tx.QueryRow(ctx, `SELECT comms_mode, capabilities FROM meters WHERE id=$1 AND tenant_id=$2`, meterID, tenant).Scan(&comms, &capsRaw); err != nil {
			return notFoundIfNoRows(err)
		}
		var caps Capabilities
		_ = json.Unmarshal(capsRaw, &caps)
		if comms != "mqtt" {
			return errf(http.StatusConflict, "meter_capability", "this meter is not connected over MQTT")
		}
		var params any = map[string]any{}
		switch in.Type {
		case "relay":
			if !caps.Relay {
				return errf(http.StatusConflict, "meter_capability", "this meter has no controllable relay")
			}
			if in.State != "on" && in.State != "off" {
				return errf(http.StatusBadRequest, "invalid_command", "relay state must be on or off")
			}
			params = map[string]string{"state": in.State}
		case "sync", "ping":
		default:
			return errf(http.StatusBadRequest, "invalid_command", "type must be relay, sync or ping")
		}
		actor := Meta(ctx).Principal.UserID
		var err error
		if out, err = enqueueCommand(ctx, tx, tenant, meterID, in.Type, params, &actor); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "meter.command." + in.Type, TargetType: "meter", TargetID: meterID.String(),
			Metadata: map[string]any{"command_id": out.ID, "state": in.State}})
	})
	return out, err
}

func (s *Service) MeterCommands(ctx context.Context, tenant, meterID uuid.UUID, limit int) ([]Command, error) {
	out := []Command{}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+commandCols+` FROM device_commands WHERE tenant_id=$1 AND meter_id=$2 ORDER BY created_at DESC LIMIT $3`,
			tenant, meterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCommand(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// OutboundCommand is a signed downlink ready to publish.
type OutboundCommand struct {
	ID      uuid.UUID
	Serial  string
	Topic   string
	Payload []byte
}

const commandResendAfter = 60 * time.Second
const maxCommandAttempts = 5

// DueCommands claims commands for the given connected serials and returns
// signed payloads. Unacknowledged commands are re-sent after a minute.
func (s *Service) DueCommands(ctx context.Context, serials []string) ([]OutboundCommand, error) {
	if len(serials) == 0 {
		return nil, nil
	}
	var out []OutboundCommand
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.id, c.type, c.params, c.expires_at, m.serial_number, m.device_secret_enc, t.vending_mode = 'sandbox'
			FROM device_commands c JOIN meters m ON m.id=c.meter_id JOIN tenants t ON t.id=c.tenant_id
			WHERE m.serial_number = ANY($1) AND c.expires_at > now() AND c.attempts < $2
			  AND (c.status='queued' OR (c.status='sent' AND c.sent_at < now() - $3::interval))
			ORDER BY c.created_at LIMIT 100 FOR UPDATE OF c SKIP LOCKED`, serials, maxCommandAttempts, fmt.Sprintf("%d seconds", int(commandResendAfter.Seconds())))
		if err != nil {
			return err
		}
		type row struct {
			id      uuid.UUID
			typ     string
			params  json.RawMessage
			expires time.Time
			serial  string
			enc     []byte
			sandbox bool
		}
		var rs []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.typ, &r.params, &r.expires, &r.serial, &r.enc, &r.sandbox); err != nil {
				rows.Close()
				return err
			}
			rs = append(rs, r)
		}
		rows.Close()
		for _, r := range rs {
			secret, err := s.Keys.Decrypt(r.enc)
			if err != nil {
				return err
			}
			data, _ := json.Marshal(map[string]any{"id": r.id, "type": r.typ, "params": r.params,
				"issued_at": s.Now().Unix(), "expires_at": r.expires.Unix(), "sandbox": r.sandbox})
			payload, err := signEnvelope(secret, r.serial, data, s.Now())
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE device_commands SET status='sent', attempts=attempts+1, sent_at=now() WHERE id=$1`, r.id); err != nil {
				return err
			}
			out = append(out, OutboundCommand{ID: r.id, Serial: r.serial, Topic: DeviceTopic(r.serial, "commands"), Payload: payload})
		}
		return nil
	})
	return out, err
}

// signEnvelope produces a downlink envelope the device verifies with
// HMAC(secret, SERIAL\nts\nnonce\ndata) — the uplink formula, reversed.
func signEnvelope(secret []byte, serial string, data []byte, now time.Time) ([]byte, error) {
	ts := fmt.Sprint(now.Unix())
	nonce := secure.RandomToken(12)
	str, _ := json.Marshal(string(data))
	return json.Marshal(Envelope{TS: ts, Nonce: nonce, Data: str,
		Sig: secure.HMACSHA256Hex(secret, serial+"\n"+ts+"\n"+nonce+"\n"+string(data))})
}

type CommandAck struct {
	CommandID uuid.UUID `json:"command_id"`
	Status    string    `json:"status"` // accepted | executed | rejected | failed
	Detail    string    `json:"detail"`
}

// HandleAck applies a device's signed acknowledgement. An executed
// credit_token is the moment a live token becomes "delivered".
func (s *Service) HandleAck(ctx context.Context, p auth.Principal, data []byte) error {
	var a CommandAck
	if err := json.Unmarshal(data, &a); err != nil {
		return errf(http.StatusBadRequest, "invalid_ack", "ack must be JSON")
	}
	switch a.Status {
	case "accepted", "executed", "rejected", "failed":
	default:
		return errf(http.StatusBadRequest, "invalid_ack", "unknown ack status")
	}
	return s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var typ, status string
		var params json.RawMessage
		err := tx.QueryRow(ctx, `SELECT type, status, params FROM device_commands WHERE id=$1 AND meter_id=$2 FOR UPDATE`,
			a.CommandID, p.MeterID).Scan(&typ, &status, &params)
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(http.StatusNotFound, "unknown_command", "command not found for this device")
		}
		if err != nil {
			return err
		}
		switch status {
		case "executed", "rejected", "failed", "expired":
			return nil // final already: duplicate ack
		}
		if _, err := tx.Exec(ctx, `UPDATE device_commands SET status=$2, result=$3, acked_at=now() WHERE id=$1`,
			a.CommandID, a.Status, truncate(a.Detail, 500)); err != nil {
			return err
		}
		if typ != "credit_token" {
			return nil
		}
		var cp struct {
			TokenID uuid.UUID `json:"token_id"`
		}
		_ = json.Unmarshal(params, &cp)
		switch a.Status {
		case "executed":
			return s.confirmTokenByID(ctx, tx, p.TenantID, p.MeterID, cp.TokenID)
		case "rejected", "failed":
			// Leave the token usable on the keypad, and tell the customer.
			if _, err := tx.Exec(ctx, `UPDATE energy_tokens SET status='issued' WHERE id=$1 AND status='submitted'`, cp.TokenID); err != nil {
				return err
			}
			return notifyMeterOwners(ctx, tx, p.TenantID, p.MeterID, "Token not applied",
				"Your meter could not apply the token remotely. You can enter it on the meter keypad.")
		}
		return nil
	})
}

func (s *Service) confirmTokenByID(ctx context.Context, tx pgx.Tx, tenant, meter, tokenID uuid.UUID) error {
	var enc []byte
	if err := tx.QueryRow(ctx, `SELECT token_enc FROM energy_tokens WHERE id=$1 AND meter_id=$2`, tokenID, meter).Scan(&enc); err != nil {
		return notFoundIfNoRows(err)
	}
	plain, err := s.Keys.Decrypt(enc)
	if err != nil {
		return err
	}
	_, err = s.confirmTokenDelivery(ctx, tx, tenant, meter, string(plain))
	return err
}

// ExpireCommands closes commands that were never acknowledged in time.
func (s *Service) ExpireCommands(ctx context.Context) error {
	return s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE device_commands SET status='expired'
			WHERE status IN ('queued','sent') AND (expires_at <= now() OR attempts >= $1 AND sent_at < now() - interval '5 minutes')
			RETURNING tenant_id, meter_id, type, params`, maxCommandAttempts)
		if err != nil {
			return err
		}
		type x struct {
			tenant, meter uuid.UUID
			typ           string
			params        json.RawMessage
		}
		var xs []x
		for rows.Next() {
			var v x
			if err := rows.Scan(&v.tenant, &v.meter, &v.typ, &v.params); err != nil {
				rows.Close()
				return err
			}
			xs = append(xs, v)
		}
		rows.Close()
		for _, v := range xs {
			if v.typ != "credit_token" {
				continue
			}
			var cp struct {
				TokenID uuid.UUID `json:"token_id"`
			}
			_ = json.Unmarshal(v.params, &cp)
			if _, err := tx.Exec(ctx, `UPDATE energy_tokens SET status='issued' WHERE id=$1 AND status='submitted'`, cp.TokenID); err != nil {
				return err
			}
			if err := notifyMeterOwners(ctx, tx, v.tenant, v.meter, "Token not delivered",
				"Your meter was unreachable. Enter the token on the meter keypad or try again when it is online."); err != nil {
				return err
			}
		}
		return nil
	})
}

// MQTTConnectionInfo is returned once at meter registration for provisioning.
type MQTTConnectionInfo struct {
	URLs         []string          `json:"urls"`
	ClientID     string            `json:"client_id"`
	Username     string            `json:"username"`
	PasswordSpec string            `json:"password_spec"`
	Topics       map[string]string `json:"topics"`
}

func (s *Service) mqttInfo(serial string) *MQTTConnectionInfo {
	if len(s.Cfg.MQTTPublicURLs) == 0 {
		return nil
	}
	return &MQTTConnectionInfo{URLs: s.Cfg.MQTTPublicURLs, ClientID: serial, Username: serial,
		PasswordSpec: `v1:<unix>:<hex HMAC-SHA256(device_secret, "mqtt-connect\n` + serial + `\n<unix>")>`,
		Topics: map[string]string{
			"telemetry": DeviceTopic(serial, "telemetry"), "acks": DeviceTopic(serial, "acks"),
			"status": DeviceTopic(serial, "status"), "commands": DeviceTopic(serial, "commands"),
		}}
}
