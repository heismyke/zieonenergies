package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"

	"github.com/heismyke/zieonenergies/internal/mqttgw"
	"github.com/heismyke/zieonenergies/pkg/zieondevice"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (e *env) startGateway() string {
	e.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := e.svc.Cfg
	cfg.MQTTTCPAddr = addr
	gw, err := mqttgw.New(e.svc, cfg, e.svc.Log)
	if err != nil {
		e.t.Fatal(err)
	}
	gw.SetDispatchInterval(100 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = gw.Run(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
	time.Sleep(200 * time.Millisecond)
	return "tcp://" + addr
}

func (e *env) mqttMeter(p partner, c customer, number string) meter {
	e.t.Helper()
	serial := "MQ-" + number
	r := e.must(e.do("POST", "/v1/platform/meters", e.platform, map[string]any{"serial_number": serial, "meter_number": number,
		"comms_mode": "mqtt", "capabilities": map[string]bool{"telemetry": true, "remote_credit": true, "relay": true, "keypad": true}}), 201)
	m := meter{ID: r.str("id"), Number: number, Serial: serial, Secret: r.str("device_secret")}
	e.must(e.do("POST", "/v1/platform/meters/"+m.ID+"/assign", e.platform, map[string]string{"tenant_id": p.ID}), 204)
	e.must(e.do("POST", "/v1/admin/meters/"+m.ID+"/assign", p.Admin, map[string]any{"user_id": c.ID, "label": "Home"}), 204)
	return m
}

func TestMQTTDeviceGateway(t *testing.T) {
	e := newEnv(t, false)
	broker := e.startGateway()
	p := e.provision("pce", nil, nil)
	c := e.customer(p.Slug, "c@pce.test")
	m := e.mqttMeter(p, c, "41000000001")
	other := e.mqttMeter(p, e.customer(p.Slug, "o@pce.test"), "41000000002")

	// Wrong secret: CONNECT refused.
	if _, err := zieondevice.Connect(broker, m.Serial, "zds_wrong", nil); err == nil {
		t.Fatal("connect with wrong secret must be refused")
	}
	// A device cannot log in as another serial (client id must equal username).
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(other.Serial).SetUsername(m.Serial).
		SetPassword(zieondevice.Password(m.Serial, []byte(m.Secret), time.Now())).SetAutoReconnect(false)
	if tok := paho.NewClient(opts).Connect(); tok.WaitTimeout(5*time.Second) && tok.Error() == nil {
		t.Fatal("client id / username mismatch must be refused")
	}
	// A stale password (outside the clock window) is refused.
	opts = paho.NewClientOptions().AddBroker(broker).SetClientID(m.Serial).SetUsername(m.Serial).
		SetPassword(zieondevice.Password(m.Serial, []byte(m.Secret), time.Now().Add(-time.Hour))).SetAutoReconnect(false)
	if tok := paho.NewClient(opts).Connect(); tok.WaitTimeout(5*time.Second) && tok.Error() == nil {
		t.Fatal("stale password must be refused")
	}

	var reject atomic.Bool
	var relayState atomic.Value
	dev, err := zieondevice.Connect(broker, m.Serial, m.Secret, func(d *zieondevice.Client, cmd zieondevice.Command) {
		switch cmd.Type {
		case "credit_token":
			if reject.Load() {
				_ = d.Ack(cmd.ID, "rejected", "STS module refused token")
				return
			}
			_ = d.Ack(cmd.ID, "executed", "ok")
		case "relay":
			var p struct{ State string }
			_ = json.Unmarshal(cmd.Params, &p)
			relayState.Store(p.State)
			_ = d.Ack(cmd.ID, "executed", "")
		default:
			_ = d.Ack(cmd.ID, "executed", "")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	getMeter := func() resp { return e.must(e.do("GET", "/v1/meters/"+m.ID, c.Token, nil), 200) }
	eventually(t, "meter online", func() bool { return getMeter().str("connectivity") == "online" })

	// Signed telemetry is ingested and becomes the displayed balance.
	if err := dev.Publish("telemetry", map[string]any{"credit_kwh": 5.0, "cumulative_kwh": 100.0, "power_status": "lightOn"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "device balance", func() bool { r := getMeter(); return r.str("balance_source") == "device" && r.num("kwh_reserve") == 5 })

	// Forged payload on the device's own topic is dropped.
	forged, _ := json.Marshal(zieondevice.Envelope{TS: fmt.Sprint(time.Now().Unix()), Nonce: "forged-nonce-1", Data: `{"credit_kwh":999}`, Sig: "00"})
	dev.Raw().Publish(zieondevice.Topic(m.Serial, "telemetry"), 1, false, forged).WaitTimeout(5 * time.Second)
	// ACL: a device may not read another meter's commands or use wildcards.
	for _, filter := range []string{zieondevice.Topic(other.Serial, "commands"), "zieon/v1/devices/#"} {
		sub := dev.Raw().Subscribe(filter, 1, nil)
		if !sub.WaitTimeout(5*time.Second) || sub.Error() != nil {
			t.Fatalf("subscribe %s: %v", filter, sub.Error())
		}
		if code := sub.(*paho.SubscribeToken).Result()[filter]; code < 0x80 {
			t.Fatalf("subscribing to %s must be refused, got code %d", filter, code)
		}
	}
	// A device that publishes into another meter's topic is disconnected and nothing is stored.
	intruder, err := zieondevice.Connect(broker, other.Serial, other.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	intruder.Raw().Publish(zieondevice.Topic(m.Serial, "telemetry"), 1, false, forged).WaitTimeout(2 * time.Second)
	eventually(t, "intruder disconnected", func() bool { return !intruder.Raw().IsConnectionOpen() })
	intruder.Close()
	time.Sleep(300 * time.Millisecond)
	if r := getMeter(); r.num("kwh_reserve") != 5 {
		t.Fatalf("forged telemetry was ingested: %v", r.num("kwh_reserve"))
	}
	var readings int
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM meter_readings WHERE meter_id=$1`, m.ID).Scan(&readings)
	if readings != 1 {
		t.Fatalf("only the one genuine reading may be stored, got %d", readings)
	}

	// Purchase -> redeem: token is pushed as a signed command and becomes
	// delivered only when the meter acks "executed".
	e.fund(c, 1000000)
	pu := e.must(e.buy(c, m, 500000), 201)
	red := e.must(e.do("POST", "/v1/tokens/redeem", c.Token, map[string]any{"meter_id": m.ID, "token": pu.str("token")}), 200)
	if red.str("status") != "submitted" || red.get("simulated") != false {
		t.Fatalf("MQTT meter redemption must wait for the device: %s", red.Raw)
	}
	eventually(t, "purchase delivered", func() bool {
		return e.must(e.do("GET", "/v1/purchases/"+pu.str("id"), c.Token, nil), 200).str("status") == "delivered"
	})
	e.must(e.do("POST", "/v1/tokens/redeem", c.Token, map[string]any{"meter_id": m.ID, "token": pu.str("token")}), 409)

	// Device rejects the next token: it returns to "issued" for keypad entry.
	reject.Store(true)
	pu2 := e.must(e.buy(c, m, 200000), 201)
	e.must(e.do("POST", "/v1/tokens/redeem", c.Token, map[string]any{"meter_id": m.ID, "token": pu2.str("token")}), 200)
	eventually(t, "rejected token reusable", func() bool {
		ts := e.must(e.do("GET", "/v1/tokens?meter_id="+m.ID, c.Token, nil), 200).list("tokens")
		return len(ts) == 1 && ts[0].(map[string]any)["status"] == "issued"
	})
	if g := e.must(e.do("GET", "/v1/purchases/"+pu2.str("id"), c.Token, nil), 200); g.str("status") != "vended" {
		t.Fatalf("rejected delivery must not mark the purchase delivered: %s", g.Raw)
	}

	// Operator relay command, audited, executed by the device.
	cmd := e.must(e.do("POST", "/v1/admin/meters/"+m.ID+"/commands", p.Admin, map[string]string{"type": "relay", "state": "off"}), 202)
	e.must(e.do("POST", "/v1/admin/meters/"+m.ID+"/commands", c.Token, map[string]string{"type": "relay", "state": "on"}), 403)
	eventually(t, "relay executed", func() bool {
		for _, x := range e.must(e.do("GET", "/v1/admin/meters/"+m.ID+"/commands", p.Admin, nil), 200).list("commands") {
			if x.(map[string]any)["id"] == cmd.str("id") {
				return x.(map[string]any)["status"] == "executed"
			}
		}
		return false
	})
	if relayState.Load() != "off" {
		t.Fatal("device did not receive relay state")
	}

	// Commands queued while offline are delivered on reconnect; disconnect marks offline.
	dev.Close()
	eventually(t, "meter offline", func() bool { return getMeter().str("connectivity") == "offline" })
	queued := e.must(e.do("POST", "/v1/admin/meters/"+m.ID+"/commands", p.Admin, map[string]string{"type": "sync"}), 202)
	dev2, err := zieondevice.Connect(broker, m.Serial, m.Secret, func(d *zieondevice.Client, cmd zieondevice.Command) { _ = d.Ack(cmd.ID, "executed", "") })
	if err != nil {
		t.Fatal(err)
	}
	defer dev2.Close()
	eventually(t, "queued command delivered after reconnect", func() bool {
		var st string
		_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT status FROM device_commands WHERE id=$1`, uuid.MustParse(queued.str("id"))).Scan(&st)
		return st == "executed"
	})
}
