// Package mqttgw is the device gateway: an embedded MQTT broker whose
// authentication and topic ACLs are backed by the meter registry. Devices
// publish signed telemetry and command acks; the gateway delivers signed
// commands queued by the API in device_commands.
//
// It runs either inside `api serve` or as its own process (`api mqtt`), so
// API restarts need not drop meter connections.
package mqttgw

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/heismyke/zieonenergies/internal/app"
	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/config"
)

type Gateway struct {
	svc      *app.Service
	log      *slog.Logger
	srv      *mqtt.Server
	interval time.Duration
	kick     chan struct{}

	mu       sync.Mutex
	sessions map[string]auth.Principal // client id (serial) -> device
}

// New builds the broker and its listeners from config.
func New(svc *app.Service, cfg config.Config, log *slog.Logger) (*Gateway, error) {
	g := &Gateway{svc: svc, log: log, interval: 2 * time.Second, kick: make(chan struct{}, 1), sessions: map[string]auth.Principal{}}
	opts := &mqtt.Options{InlineClient: true, Logger: log.With("component", "mqtt")}
	caps := mqtt.NewDefaultServerCapabilities()
	caps.MaximumPacketSize = 16 * 1024
	caps.MaximumClients = 50000
	caps.RetainAvailable = 0 // nothing on device topics should persist in the broker
	opts.Capabilities = caps
	g.srv = mqtt.New(opts)
	if err := g.srv.AddHook(&hook{g: g}, nil); err != nil {
		return nil, err
	}
	if cfg.MQTTTCPAddr != "" {
		if err := g.srv.AddListener(listeners.NewTCP(listeners.Config{ID: "tcp", Address: cfg.MQTTTCPAddr})); err != nil {
			return nil, err
		}
	}
	if cfg.MQTTTLSAddr != "" {
		certs := &certReloader{certGlob: cfg.MQTTTLSCert, keyGlob: cfg.MQTTTLSKey}
		if _, err := certs.get(nil); err != nil {
			return nil, err
		}
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}
		if err := g.srv.AddListener(listeners.NewTCP(listeners.Config{ID: "tls", Address: cfg.MQTTTLSAddr, TLSConfig: tlsCfg})); err != nil {
			return nil, err
		}
	}
	if cfg.MQTTWSAddr != "" {
		if err := g.srv.AddListener(listeners.NewWebsocket(listeners.Config{ID: "ws", Address: cfg.MQTTWSAddr})); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// SetDispatchInterval is used by tests.
func (g *Gateway) SetDispatchInterval(d time.Duration) { g.interval = d }

// Run serves until ctx is cancelled.
func (g *Gateway) Run(ctx context.Context) error {
	if err := g.srv.Serve(); err != nil {
		return err
	}
	g.log.Info("mqtt gateway listening")
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return g.srv.Close()
		case <-t.C:
		case <-g.kick:
		}
		g.dispatch(ctx)
	}
}

func (g *Gateway) nudge() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

func (g *Gateway) connectedSerials() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.sessions))
	for id := range g.sessions {
		out = append(out, id)
	}
	return out
}

func (g *Gateway) dispatch(ctx context.Context) {
	cmds, err := g.svc.DueCommands(ctx, g.connectedSerials())
	if err != nil {
		g.log.Error("mqtt: load commands", "err", err)
		return
	}
	for _, c := range cmds {
		if err := g.srv.Publish(c.Topic, c.Payload, false, 1); err != nil {
			g.log.Error("mqtt: publish command", "command", c.ID, "err", err)
			continue
		}
		g.log.Info("mqtt: command sent", "command", c.ID, "serial", c.Serial)
	}
}

// ---------------------------------------------------------------------------
// Broker hook
// ---------------------------------------------------------------------------

type hook struct {
	mqtt.HookBase
	g *Gateway
}

func (h *hook) ID() string { return "zieon-devices" }

func (h *hook) Provides(b byte) bool {
	return bytes.Contains([]byte{mqtt.OnConnectAuthenticate, mqtt.OnACLCheck, mqtt.OnPublish,
		mqtt.OnSessionEstablished, mqtt.OnDisconnect}, []byte{b})
}

func ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (h *hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	ctx, cancel := ctxTimeout()
	defer cancel()
	p, err := h.g.svc.AuthenticateMQTT(ctx, cl.ID, string(pk.Connect.Username), string(pk.Connect.Password))
	if err != nil {
		h.g.log.Warn("mqtt: connect refused", "client", cl.ID, "remote", cl.Net.Remote)
		return false
	}
	h.g.mu.Lock()
	h.g.sessions[strings.ToUpper(cl.ID)] = p
	h.g.mu.Unlock()
	return true
}

func (h *hook) principal(cl *mqtt.Client) (auth.Principal, bool) {
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	p, ok := h.g.sessions[strings.ToUpper(cl.ID)]
	return p, ok
}

// OnACLCheck pins each device to its own topics: it may publish telemetry,
// acks and status, and subscribe only to its own commands. No wildcards.
func (h *hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if cl.Net.Inline {
		return true
	}
	serial := strings.ToUpper(cl.ID)
	if write {
		for _, k := range []string{"telemetry", "acks", "status"} {
			if topic == app.DeviceTopic(serial, k) {
				return true
			}
		}
		return false
	}
	return topic == app.DeviceTopic(serial, "commands")
}

func (h *hook) OnSessionEstablished(cl *mqtt.Client, pk packets.Packet) {
	if p, ok := h.principal(cl); ok {
		ctx, cancel := ctxTimeout()
		defer cancel()
		if err := h.g.svc.SetDeviceConnectivity(ctx, p, true); err != nil {
			h.g.log.Error("mqtt: mark online", "client", cl.ID, "err", err)
		}
		h.g.nudge() // deliver anything queued while the meter was away
	}
}

func (h *hook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	if cl.IsTakenOver() {
		return // a newer connection for the same serial replaced this one
	}
	p, ok := h.principal(cl)
	if !ok {
		return
	}
	h.g.mu.Lock()
	delete(h.g.sessions, strings.ToUpper(cl.ID))
	h.g.mu.Unlock()
	ctx, cancel := ctxTimeout()
	defer cancel()
	if err := h.g.svc.SetDeviceConnectivity(ctx, p, false); err != nil {
		h.g.log.Error("mqtt: mark offline", "client", cl.ID, "err", err)
	}
}

// OnPublish processes device uplinks synchronously. Valid or permanently
// invalid messages are acknowledged and not forwarded to any subscriber;
// transient failures (database) are left unacknowledged so QoS 1 redelivers.
func (h *hook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if cl.Net.Inline {
		return pk, nil // our own downlink
	}
	serial := strings.ToUpper(cl.ID)
	kind := strings.TrimPrefix(pk.TopicName, app.TopicPrefix+serial+"/")
	if kind == "status" {
		return pk, packets.CodeSuccessIgnore
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	msg, err := h.g.svc.VerifyEnvelope(ctx, serial, pk.Payload)
	if err == nil {
		switch kind {
		case "telemetry":
			var t app.Telemetry
			if err = decodeStrict(msg.Data, &t); err == nil {
				_, err = h.g.svc.IngestTelemetry(ctx, msg.Principal, t)
			}
		case "acks":
			err = h.g.svc.HandleAck(ctx, msg.Principal, msg.Data)
		default:
			err = errors.New("unknown topic")
		}
	}
	if err != nil {
		var ae *app.Error
		if errors.As(err, &ae) || err.Error() == "unknown topic" {
			h.g.log.Warn("mqtt: message rejected", "serial", serial, "topic", kind, "reason", err.Error())
			return pk, packets.CodeSuccessIgnore
		}
		h.g.log.Error("mqtt: message processing failed", "serial", serial, "topic", kind, "err", err)
		return pk, packets.ErrRejectPacket
	}
	return pk, packets.CodeSuccessIgnore
}

// ---------------------------------------------------------------------------
// TLS certificates (re-read when the files change, e.g. ACME renewal)
// ---------------------------------------------------------------------------

type certReloader struct {
	certGlob, keyGlob string
	mu                sync.Mutex
	cert              *tls.Certificate
	modTime           time.Time
	checked           time.Time
}

func newest(glob string) (string, time.Time, error) {
	matches, err := filepath.Glob(glob)
	if err != nil || len(matches) == 0 {
		return "", time.Time{}, errors.New("no file matches " + glob)
	}
	var best string
	var bestT time.Time
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.ModTime().After(bestT) {
			best, bestT = m, st.ModTime()
		}
	}
	return best, bestT, nil
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && time.Since(c.checked) < time.Minute {
		return c.cert, nil
	}
	c.checked = time.Now()
	certPath, mt, err := newest(c.certGlob)
	if err != nil {
		if c.cert != nil {
			return c.cert, nil
		}
		return nil, err
	}
	if c.cert != nil && !mt.After(c.modTime) {
		return c.cert, nil
	}
	keyPath, _, err := newest(c.keyGlob)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		if c.cert != nil {
			return c.cert, nil // keep serving the old cert during a partial write
		}
		return nil, err
	}
	c.cert, c.modTime = &pair, mt
	return c.cert, nil
}
