# MQTT Device Protocol (v1)

This document is for the Zieon embedded/firmware team. It defines how a Zieon smart meter connects to the platform over MQTT.

**Status:** the server side, a reference client and a simulator are implemented and tested (`TestMQTTDeviceGateway`). **No firmware implements this protocol yet.**

## Endpoints (pilot)
| Transport | URL | Use for |
|---|---|---|
| MQTT over TLS 1.2+ | `ssl://zieon-api.198.199.65.142.sslip.io:8883` | GSM modules (e.g. SIM7600 `AT+CMQTT*` with SSL), and any MCU with a TLS stack |
| MQTT over WebSocket + TLS | `wss://zieon-api.198.199.65.142.sslip.io/mqtt` (subprotocol `mqtt`) | ESP32 / Wi-Fi meters, and networks that only allow 443 |

Protocol details:
- **Versions:** MQTT 3.1.1 and 5 are both supported.
- **QoS:** use QoS 1 everywhere.
- **Maximum packet size:** 16 KiB.
- **Certificate:** a public Let's Encrypt certificate. Devices need the ISRG Root X1 CA in their trust store.

## 1. Identity and provisioning
Registering a meter (`POST /v1/platform/meters` with `"comms_mode":"mqtt"`) returns, **once**:
- `serial_number`, e.g. `ZE-000123`. It is also the MQTT client id and username.
- `device_secret`, e.g. `zds_…`, a 43-character string. Write it into the secure element or encrypted flash at manufacture. **Never log it or print it on labels.**
- `mqtt`: the URLs, topics and password formula.

## 2. Connecting
- `client_id = username = SERIAL` (uppercase). A mismatch is refused.
- `password = "v1:" + unix + ":" + hex(HMAC_SHA256(device_secret, "mqtt-connect\n" + SERIAL + "\n" + unix))`
  - `unix` is the current Unix time in seconds. It must be within **±5 minutes** of server time, so sync the clock first: GSM `AT+CCLK`/NITZ, or NTP on Wi-Fi.
  - **Generate a fresh password for every connection attempt.** The secret itself is never sent.
- `clean_session = false` and `keepalive = 60 s`.
- Last-will: topic `zieon/v1/devices/SERIAL/status`, payload `offline`, QoS 1.

The broker marks the meter **online** when it connects and **offline** when it disconnects. The disconnect also emits a `meter.offline` webhook to the partner. Separately, a meter silent for 30 minutes is flagged by the server.

## 3. Topics (per meter; no wildcards allowed)
| Direction | Topic | Content |
|---|---|---|
| device → cloud | `zieon/v1/devices/SERIAL/telemetry` | Signed envelope: readings |
| device → cloud | `zieon/v1/devices/SERIAL/acks` | Signed envelope: command result |
| device → cloud | `zieon/v1/devices/SERIAL/status` | `online` / `offline` (informational) |
| cloud → device | `zieon/v1/devices/SERIAL/commands` | Signed envelope: command (subscribe QoS 1) |

The broker enforces these rules:
- A device may only publish and subscribe to **its own** topics.
- Publishing to any other topic **disconnects** the device.
- Subscribing to another meter's topic, or using `#`/`+` wildcards, is refused with reason code `0x80`.

## 4. Signed envelope (both directions)
```json
{"ts":"1794510000","nonce":"9f2c41ab77e0d3c1a5b2","data":"{\"credit_kwh\":12.4}","sig":"<64 hex>"}
```
- **`data`** is the JSON document **as a string**. You sign exactly those bytes.
- **`sig`** is `hex(HMAC_SHA256(device_secret, SERIAL + "\n" + ts + "\n" + nonce + "\n" + data))`.
- **`ts`:** ±5 minutes.
- **`nonce`:** 8–64 characters and **never reused**. 12 random bytes as hex is fine. The server rejects replays.
- **Devices must verify `sig` on every command** before acting on it. Drop anything that fails, even though it came through the authenticated broker.

## 5. Telemetry (`data` of a telemetry message)
```json
{"credit_kwh": 12.4, "cumulative_kwh": 10234.125, "voltage_v": 229.8, "current_a": 3.1,
 "power_status": "lightOn", "tamper": false, "firmware_version": "1.4.2",
 "recorded_at": "2026-11-12T18:30:00Z", "accepted_tokens": []}
```
- **Field rules:** all fields are optional, and unknown fields are rejected.
- **`power_status`** is one of `lightOn`, `lowCurrent` or `noLight`.
- **`recorded_at`** may be up to 72 hours in the past. Use it to buffer readings while offline and send them oldest first.
- **`cumulative_kwh`** is the energy register. Consumption charts come from its differences, so never reset it.
- **`credit_kwh`** becomes the balance shown in apps (`balance_source: "device"`). It also drives low-balance alerts and auto top-up.
- **Recommended cadence:** every 15 minutes. Also send immediately after credit changes, tamper, or relay changes.

## 6. Commands (`data` of a command message)
```json
{"id":"6f0c…","type":"credit_token","params":{"token":"99123412341234123412","token_id":"…","energy_kwh":25},
 "issued_at":1794510000,"expires_at":1794596400,"sandbox":true}
```
| `type` | `params` | Device action |
|---|---|---|
| `credit_token` | `token` (20 digits), `token_id`, `energy_kwh` | Pass the token to the STS metrology/token module. Ack `executed` only if the module accepts it; otherwise ack `rejected` with the module's reason. |
| `relay` | `state`: `on` / `off` | Switch the load relay. Ack `executed` after switching. |
| `sync` | — | Ack, then publish telemetry immediately. |
| `ping` | — | Ack `executed`. |

Delivery rules:
- **Idempotency:** commands are delivered **at least once**. They are re-sent after 60 s without an ack, up to 5 attempts. Store the last ~50 executed `id`s. If an `id` you already executed arrives again, ack `executed` again **without re-applying it**. Re-applying would double-credit.
- **Expiry:** if `now > expires_at`, ack `rejected` with detail `expired` and do nothing.
- **`sandbox: true`:** marks platform-sandbox tokens (they start with `99`). Production firmware **must reject** them. Development firmware may simulate acceptance.
- **Offline meters:** commands queued while a meter is offline are delivered when it reconnects, until they expire. Token credits expire after 24 hours, relay commands after 10 minutes.

## 7. Acknowledgement (`data` of an ack message)
```json
{"command_id":"6f0c…","status":"executed","detail":"token accepted"}
```
`status` is one of:
- **`accepted`** (received, still working)
- **`executed`**
- **`rejected`** (not applied)
- **`failed`** (an error)

What the server does:
- On `credit_token` **executed**, it marks the token delivered, credits the platform energy ledger, and sets the purchase to `delivered`.
- On **rejected or failed**, it returns the token to the customer for keypad entry and notifies them.

## 8. Reference implementation
- **`pkg/zieondevice`** is a Go client implementing all of the above. Read it as the executable spec.
- **`cmd/devicesim`** is a meter simulator:
  ```bash
  go run ./cmd/devicesim -broker ssl://zieon-api.198.199.65.142.sslip.io:8883 -serial ZE-000123 -secret zds_… -interval 30s
  ```

Firmware pseudo-code (ESP32 with mbedTLS; a SIM7600 is equivalent, using `AT+CMQTTCONNECT`/`AT+CMQTTPUB` with SSL context):
```c
// password
snprintf(msg, sizeof msg, "mqtt-connect\n%s\n%lu", SERIAL, now);
hmac_sha256_hex(secret, msg, sig_hex);                      // mbedtls_md_hmac(...)
snprintf(password, sizeof password, "v1:%lu:%s", now, sig_hex);

// publish telemetry
snprintf(data, sizeof data, "{\"credit_kwh\":%.3f,\"cumulative_kwh\":%.3f}", credit, reg);
random_hex(nonce, 12);
snprintf(msg, sizeof msg, "%s\n%lu\n%s\n%s", SERIAL, now, nonce, data);
hmac_sha256_hex(secret, msg, sig_hex);
// JSON-escape `data` when embedding it as a string:
snprintf(env, sizeof env, "{\"ts\":\"%lu\",\"nonce\":\"%s\",\"data\":\"%s\",\"sig\":\"%s\"}",
         now, nonce, json_escape(data), sig_hex);
mqtt_publish(TOPIC_TELEMETRY, env, QOS1);

// on command: parse env, recompute sig over (SERIAL\nts\nnonce\ndata), constant-time compare,
// then parse `data`, check id cache + expires_at, execute, publish ack (signed the same way).
```

## 9. Security notes
- **TLS** protects the transport. The **HMAC envelope** protects integrity end to end, so even a compromised broker cannot forge credit commands without the device secret.
- **Secret rotation:** not yet implemented (needs a `rotate_secret` command and a secure-element write). Until then, a leaked secret requires re-registering the meter.
- **Not yet built:**
  - firmware OTA
  - remote unit debit for Spark Share between MQTT meters (today, shares move platform-ledger units only)
  - mutual-TLS client certificates (possible future hardening, replacing the HMAC password)
