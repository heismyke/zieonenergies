// Command devicesim simulates a Zieon smart meter over MQTT. It reports signed
// telemetry, executes signed commands and acknowledges them, exactly as
// firmware must. Use it for gateway testing and as a firmware reference.
//
//	go run ./cmd/devicesim -broker ssl://zieon-api.198.199.65.142.sslip.io:8883 \
//	    -serial SBX-ABC123 -secret zds_... -credit 20 -interval 30s
package main

import (
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/heismyke/zieonenergies/pkg/zieondevice"
)

func main() {
	broker := flag.String("broker", "tcp://127.0.0.1:1883", "tcp://, ssl:// or wss:// broker URL")
	serial := flag.String("serial", "", "meter serial number")
	secret := flag.String("secret", os.Getenv("DEVICE_SECRET"), "device secret (or DEVICE_SECRET)")
	credit := flag.Float64("credit", 20, "starting credit in kWh")
	interval := flag.Duration("interval", 30*time.Second, "telemetry interval")
	flag.Parse()
	if *serial == "" || *secret == "" {
		log.Fatal("-serial and -secret are required")
	}

	var mu sync.Mutex
	creditKwh, cumulative, relayOn := *credit, 1000.0, true
	var accepted []string
	report := func(c *zieondevice.Client) {
		mu.Lock()
		t := map[string]any{"credit_kwh": creditKwh, "cumulative_kwh": cumulative, "voltage_v": 228 + rand.Float64()*4,
			"current_a": 2.5, "power_status": map[bool]string{true: "lightOn", false: "noLight"}[relayOn],
			"firmware_version": "sim-1.0", "accepted_tokens": accepted}
		accepted = nil
		mu.Unlock()
		if err := c.Publish("telemetry", t); err != nil {
			log.Printf("telemetry: %v", err)
		} else {
			log.Printf("telemetry sent: credit=%.2f kWh", t["credit_kwh"])
		}
	}

	client, err := zieondevice.Connect(*broker, *serial, *secret, func(c *zieondevice.Client, cmd zieondevice.Command) {
		log.Printf("command %s %s sandbox=%v", cmd.ID, cmd.Type, cmd.Sandbox)
		switch cmd.Type {
		case "credit_token":
			var p struct {
				Token     string  `json:"token"`
				EnergyKwh float64 `json:"energy_kwh"`
			}
			_ = json.Unmarshal(cmd.Params, &p)
			// Real firmware hands the 20-digit token to the STS metrology module
			// and acks "executed" only if the module accepts it.
			mu.Lock()
			creditKwh += p.EnergyKwh
			mu.Unlock()
			_ = c.Ack(cmd.ID, "executed", "token accepted")
			report(c)
		case "relay":
			var p struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(cmd.Params, &p)
			mu.Lock()
			relayOn = p.State == "on"
			mu.Unlock()
			_ = c.Ack(cmd.ID, "executed", "relay "+p.State)
		case "sync":
			_ = c.Ack(cmd.ID, "executed", "")
			report(c)
		default:
			_ = c.Ack(cmd.ID, "executed", "")
		}
	})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Close()
	log.Printf("connected as %s", *serial)
	report(client)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	tick := time.NewTicker(*interval)
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			mu.Lock()
			if relayOn && creditKwh > 0 {
				used := 0.05 + rand.Float64()*0.1
				creditKwh, cumulative = max(0, creditKwh-used), cumulative+used
			}
			mu.Unlock()
			report(client)
		}
	}
}
