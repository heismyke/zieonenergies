# Meter Vending Flow

## Important limitation
The platform contains **no STS key material, no token-generation algorithm and no DisCo vending integration**. The only vending provider is `vending.Sandbox`:
- Its tokens are 20 digits, always start with `99`, are flagged `"sandbox": true`, and work on **no real meter**.
- Meter numbers ending in `0000` are rejected, so failure paths can be exercised.
- `APP_ENV=production` removes the sandbox provider entirely, and setting a tenant to `vending_mode=live` fails while no live provider exists.

A real route needs one of the following:
1. **Zieon meters:** an STS-compliant vending system using an accredited **Key Management Centre**. Zieon's meters need Supply Group Codes and key revisions registered with the STS Association. The vending system then issues tokens under Zieon's own (or a licensed operator's) authority.
2. **DisCo meters:** a vending agreement with each DisCo, or with a licensed aggregator. Under that agreement the DisCo's system issues the token.

Both plug into `vending.Provider`. The contract is: `Vend` is idempotent on our `Reference`, and `Lookup(reference)` reports the outcome for reconciliation.

## Purchase state machine
```
                 (card / transfer)                      ┌──────────────► needs_review ──┐
   create ──► payment_pending ──webhook/verify──► vending ──ok──► vended ──meter ack──► delivered
     │              │                               │  ▲                                  │
     │ (wallet /    └──fail/expire──► payment_failed │  └──retry (same ref)── needs_review  │
     │  rewards)                                     └──rejected──► vend_failed ──► refunded ◄┘ (operator)
     └──────────────────────────────────────────────► vending
```
- **Allowed transitions** are enforced in code (`canTransition`) and by `UPDATE … WHERE status = <from>`. A concurrent worker can never apply the same transition twice.
- **Illegal transitions are rejected** (unit-tested), for example:
  - `refunded → vending`
  - `vended → refunded`
  - `delivered → refunded`

## Step by step (`app.Purchase`)
1. **Validate.** Check the payment method is enabled for the tenant, the meter is linked to the caller and active, and the amount is within the tariff's min and max.
2. **Quote** (`computeQuote`).
   - Fee comes off the top, and VAT is inclusive.
   - `energy_value = (amount − fee) − vat`, and `energy_wh = energy_value × 1000 / rate`.
   - Rewards never change this.
3. **Fund**, inside one DB transaction with the vend row:
   - **Rewards leg:** consume grants FIFO by expiry, then post `reward → clearing`.
   - **Wallet leg:** post `wallet → clearing`. A `CHECK balance >= 0` makes concurrent overspend impossible; 10 parallel purchases were tested.
   - **External leg** (card / transfer): create a `payments` row. The provider is called **after** commit, and the status becomes `payment_pending`.
4. **Vend**, outside the transaction:
   - Lock the row, increment `attempts`, and call `Provider.Vend(reference = txn id)`.
   - Apply the result only if the row is still `vending`.
5. **Outcome.**
   - **Success:** encrypt and store the token (SHA-256 hash for lookup), set `vended`, grant tier rewards, notify, and emit `purchase.vended`.
   - **Definitive rejection:** `vend_failed → refunded`. All legs are returned. Card and transfer legs are refunded **to the wallet**; cash-out to the card is an operator action.
   - **Unknown** (timeout or network): stay `vending` and record the reason. Reconciliation takes over.
6. **Delivery (DIU)** via `POST /v1/tokens/redeem`.
   - **Sandbox token:** credit the platform energy ledger, mark `delivered`, and return `simulated: true`.
   - **Live token:** mark `submitted`. It becomes `delivered` **only** when the meter reports the token in `accepted_tokens` in signed telemetry. The platform never claims credit it cannot observe.

## Payments and webhooks
- There is one confirmation path, `ConfirmPayment(provider, reference, amount)`, used by:
  - inbound webhooks
  - the reconciliation worker (`Verify`)
  - the sandbox provider
- Inbound webhooks:
  - **Paystack:** verifies `X-Paystack-Signature` (HMAC-SHA512 of the raw body). It then **re-verifies via the Paystack API** before crediting, because the webhook body alone is not trusted for the amount.
  - **Sandbox:** verifies `Zieon-Signature` (HMAC-SHA256 with timestamp) and is disabled in production.
- Duplicate protection:
  - **Duplicate events** are dropped by `webhook_events (provider, event_id)`.
  - **Duplicate confirmations** for the same payment are no-ops because the payment row is locked and `status <> 'pending'`.
  - Tested: one token after two identical and one distinct success webhook.
- **Amount mismatch:** the payment is marked failed, nothing is credited, and an audit row is written (tested).

## Reconciliation workers (`app/workers.go`)
| Job | Every | Action |
|---|---|---|
| `reconcile_vends` | 30 s | For vends stuck in `vending` for more than 2 minutes, call `Lookup`. If found, complete. If not found and attempts < 3, retry with the same reference. If not found after 3 attempts, reject and refund. If still unknown after 30 minutes, move to `needs_review` (no money moves without a human). |
| `reconcile_payments` | 1 min | For payments pending more than 10 minutes, call `Verify`. Confirm, fail, or expire after 24 hours. |
| `auto_topup` | 30 s | Execute queued wallet top-ups raised by low-credit telemetry. |
| `meter_offline` | 1 min | Mark telemetry meters silent for more than 30 minutes as offline, raise an alert and emit a webhook. |
| `expire_rewards` | 10 min | Return unspent expired grants to the partner pool. |
| `webhooks` | 5 s | Deliver the outbox with backoff of 30 s × 2ⁿ, capped at 6 h, for 8 attempts. |
| `cleanup` | 1 h | Purge nonces, idempotency keys, embed codes and old refresh tokens. |

Operator recovery: `POST /v1/platform/tenants/{id}/transactions/{txn}/resolve {"action":"refund"|"retry"}`. Only valid from `needs_review`, and audited.

## Device protocol (proposed, server implemented, firmware pending)
```
POST /v1/devices/telemetry
X-Device-Serial: ZE-000123
X-Device-Timestamp: 1794500000          (unix seconds, ±5 min)
X-Device-Nonce: 8–64 chars, single use
X-Device-Signature: hex(HMAC-SHA256(device_secret, serial \n timestamp \n nonce \n body))
{"credit_kwh": 12.4, "cumulative_kwh": 10234.1, "voltage_v": 229.8, "current_a": 3.1,
 "power_status": "lightOn", "tamper": false, "firmware_version": "1.4.2",
 "accepted_tokens": ["9912…"]}
```
- **Device secret:** generated at registration, shown once (to be written into firmware or a secure element at manufacture), and stored AES-GCM encrypted.
- **Meters without communications** (`comms_mode = none`) have no secret and cannot post telemetry. Their balance is shown as `balance_source = platform_ledger`.
- **Not built:** downlink commands (remote credit, relay disconnect) need the hardware's protocol (MQTT/GSM) and are **BLOCKED** on firmware.
