// Package zieondevice is a reference Go client for the Zieon MQTT device
// protocol. Firmware teams can read it as an executable specification; the
// simulator (cmd/devicesim) and the gateway tests use it.
package zieondevice

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

type Envelope struct {
	TS    string `json:"ts"`
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
	Sig   string `json:"sig"`
}

type Command struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"` // credit_token | relay | sync | ping
	Params    json.RawMessage `json:"params"`
	IssuedAt  int64           `json:"issued_at"`
	ExpiresAt int64           `json:"expires_at"`
	Sandbox   bool            `json:"sandbox"`
}

type Client struct {
	Serial string
	Secret []byte
	mqtt   paho.Client
	seen   map[string]bool // command ids already executed (idempotency)
}

func mac(secret []byte, msg string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func nonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Password returns the time-bound MQTT password: v1:<unix>:<hmac>.
func Password(serial string, secret []byte, now time.Time) string {
	ts := fmt.Sprint(now.Unix())
	return "v1:" + ts + ":" + mac(secret, "mqtt-connect\n"+serial+"\n"+ts)
}

func Topic(serial, kind string) string { return "zieon/v1/devices/" + serial + "/" + kind }

// Connect dials broker (tcp://, ssl:// or wss://) and subscribes to commands.
func Connect(broker, serial, secret string, onCommand func(*Client, Command)) (*Client, error) {
	c := &Client{Serial: strings.ToUpper(serial), Secret: []byte(secret), seen: map[string]bool{}}
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(c.Serial).SetUsername(c.Serial).
		SetCleanSession(false).SetAutoReconnect(true).SetConnectTimeout(10*time.Second).
		SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}).
		SetWill(Topic(c.Serial, "status"), "offline", 1, false)
	// A fresh password on every (re)connect keeps it inside the clock window.
	opts.SetCredentialsProvider(func() (string, string) { return c.Serial, Password(c.Serial, c.Secret, time.Now()) })
	opts.SetOnConnectHandler(func(m paho.Client) {
		m.Subscribe(Topic(c.Serial, "commands"), 1, func(_ paho.Client, msg paho.Message) {
			cmd, err := c.verify(msg.Payload())
			if err != nil {
				return // unsigned or forged: ignore, never execute
			}
			if c.seen[cmd.ID] {
				_ = c.Ack(cmd.ID, "executed", "duplicate delivery")
				return
			}
			if cmd.ExpiresAt > 0 && time.Now().Unix() > cmd.ExpiresAt {
				_ = c.Ack(cmd.ID, "rejected", "expired")
				return
			}
			c.seen[cmd.ID] = true
			if onCommand != nil {
				onCommand(c, cmd)
			}
		})
	})
	c.mqtt = paho.NewClient(opts)
	t := c.mqtt.Connect()
	if !t.WaitTimeout(15*time.Second) || t.Error() != nil {
		if t.Error() != nil {
			return nil, t.Error()
		}
		return nil, errors.New("connect timeout")
	}
	return c, nil
}

func (c *Client) verify(payload []byte) (Command, error) {
	var e Envelope
	if err := json.Unmarshal(payload, &e); err != nil {
		return Command{}, err
	}
	want := mac(c.Secret, c.Serial+"\n"+e.TS+"\n"+e.Nonce+"\n"+e.Data)
	if !hmac.Equal([]byte(want), []byte(e.Sig)) {
		return Command{}, errors.New("bad signature")
	}
	var cmd Command
	return cmd, json.Unmarshal([]byte(e.Data), &cmd)
}

// Publish sends a signed envelope to telemetry or acks.
func (c *Client) Publish(kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	ts, n := fmt.Sprint(time.Now().Unix()), nonce()
	env, _ := json.Marshal(Envelope{TS: ts, Nonce: n, Data: string(raw),
		Sig: mac(c.Secret, c.Serial+"\n"+ts+"\n"+n+"\n"+string(raw))})
	t := c.mqtt.Publish(Topic(c.Serial, kind), 1, false, env)
	if !t.WaitTimeout(10 * time.Second) {
		return errors.New("publish timeout")
	}
	return t.Error()
}

func (c *Client) Ack(commandID, status, detail string) error {
	return c.Publish("acks", map[string]string{"command_id": commandID, "status": status, "detail": detail})
}

// Raw exposes the underlying MQTT client (tests use it for negative cases).
func (c *Client) Raw() paho.Client { return c.mqtt }

func (c *Client) Close() { c.mqtt.Disconnect(250) }
