# Nigeria Regulatory Readiness

> **Status: no regulatory approval, licence, certification or registration is evidenced in this repository or workspace.** The workspace contains CAC registration documents for Zieon Energies Ltd (`ZIEON ENERGIES LTD CERTIFICATE].pdf`, `…MERMAT.pdf`, `…Status Report CAC.pdf`). Company incorporation is **not** a sector licence.
>
> This is an engineering-led readiness assessment, **not legal advice**. Every item marked *Verify* must be confirmed with Nigerian energy and data-protection counsel and the relevant regulator before launch. Regulations in this sector changed materially after the Electricity Act 2023, which moved regulatory powers to states that set up their own regulators.

Legend:
- **Likely required**: strong basis that the obligation applies to the activity.
- **Verify**: applicability or the current instrument must be confirmed.
- **N/A**: does not apply to this business model.

## 1. Regulators and instruments in scope
| Body / instrument | Relevance |
|---|---|
| **NERC** (Nigerian Electricity Regulatory Commission) and its successor state regulators | Licensing of distribution, metering and vending activities. Meter Asset Provider (MAP) regulations. Metering Code. Mini-grid regulations. Customer protection. |
| **State electricity regulators** (e.g. Lagos, Enugu, Ondo, Ekiti, Oyo, Imo and others that have assumed jurisdiction) | Since the Electricity Act 2023, they regulate intra-state markets. **Verify which regulator governs each pilot site** (PCE's and Red's locations). |
| **NEMSA** (Nigerian Electricity Management Services Agency) | Test and certification of meters before installation. Inspection of installations. |
| **SON** (Standards Organisation of Nigeria) | MANCAP for locally manufactured products. SONCAP for imported products or components. |
| **NDPC** (Nigeria Data Protection Commission), under the Nigeria Data Protection Act 2023 | Lawful basis, notices, DPIA, breach notification, registration as a data controller/processor "of major importance", cross-border transfer rules. |
| **CBN** (Central Bank of Nigeria) | Payment service licensing, stored value / e-money, agent and wallet rules. |
| **STS Association** / IEC 62055-41/51/52 | STS token standard, Key Management Centre, Supply Group Codes, meter key loading. |

## 2. Obligations by business model

### A. Private solar or estate metering (e.g. Red Real Estate estates on private generation)
| Item | Assessment |
|---|---|
| Mini-grid / embedded generation permit or registration | **Verify.** Depends on capacity thresholds and on state vs NERC jurisdiction. Generation and distribution inside an estate may need a permit or registration even when the system is privately owned. |
| Tariff approval | **Verify.** Regulators may require tariff methodology approval for supply to third-party occupants. The benefits engine deliberately keeps tariffs separate from rewards so the regulated tariff can be shown. |
| NEMSA meter certification | **Likely required** for meters used for billing, even on private networks. **Verify.** |
| Customer protection (complaints, disconnection notice) | **Likely required.** The platform has support tickets but no regulated complaints workflow or SLA reporting. |

### B. Public-grid DisCo metering (e.g. PCE customers on DisCo supply)
| Item | Assessment |
|---|---|
| Meter Asset Provider (MAP) status or contract with a DisCo | **Likely required** for Zieon to supply and install meters on DisCo networks. MAPs operate under regulator-issued regulations and DisCo procurement. |
| DisCo vending authorisation (third-party vendor agreement) | **Likely required.** Selling units for DisCo meters needs agreements with each DisCo or a licensed aggregator. **This blocks live vending for external meters.** |
| STS keys for DisCo meters | **Not ours.** DisCo tokens are issued by the DisCo's vending system. Zieon must never hold DisCo keys. |
| Metering Code compliance (accuracy class, sealing, testing intervals) | **Likely required.** |
| Settlement and reconciliation with the DisCo | **Likely required** contractually. The platform ledger supports it, but no DisCo settlement export exists yet. |

### C. Software-only white-label services (platform used by PCE and Red without Zieon hardware)
| Item | Assessment |
|---|---|
| Electricity licence | **Verify.** A pure software provider may not need one, but the **vending/collection role** may be regulated depending on who is the merchant of record. Structure contracts so the licensed party (DisCo, MAP or mini-grid operator) is the seller. |
| Customer wallet (stored naira balance) | **High risk, Verify.** Holding customer funds that can be spent later resembles e-money / stored value under CBN rules. Options are to: (a) obtain the appropriate CBN licence; (b) hold wallets with a licensed bank, PSB or MMO partner (virtual accounts and ledger sync); or (c) disable `wallet` and use pay-per-purchase checkout only. **Feature flag `wallet` lets us switch it off per tenant today.** |
| Payment processing | Use a **CBN-licensed PSP** (e.g. Paystack, implemented but not live-tested). Zieon does not touch card data. |
| Partner-funded rewards | **Verify.** The structure is promotional funding held by Zieon for the partner. Confirm it is not deposit-taking, and document it in the partner agreement. Consumer-protection disclosure of reward terms is needed. |
| NDPA | **Likely required.** As processor for partners and controller for Zenerlink customers, Zieon needs: data processing agreements with PCE and Red, a privacy notice, a DPIA, breach procedures (the Act's 72-hour notification), and records of processing. **NDPC registration as a data controller/processor of major importance: Verify** thresholds and fees under current NDPC guidance. |
| Data residency / cross-border transfer | **Verify.** The current host (DigitalOcean) location must be confirmed. Cross-border transfer requires an adequacy basis or safeguards under the NDPA. |

### D. Physical meter manufacturing and installation (Zieon hardware)
| Item | Assessment |
|---|---|
| SON MANCAP (local manufacture) / SONCAP (imported parts or finished meters) | **Likely required.** |
| NEMSA type test and certification, and installation inspection | **Likely required** before any meter is used for billing. |
| STS compliance and Key Management Centre registration | **Likely required** for prepaid STS meters: STS Association membership or a licensed KMC, meter keys, Supply Group Codes. **Zieon must not create its own key management outside an accredited KMC.** |
| Radio / telecoms type approval (GSM or Wi-Fi modules) | **Verify.** NCC type approval for communication devices. |
| Electrical installation competence | **Verify.** NEMSA-certified installers or electrical contractors. |

## 3. What the platform already does to support compliance
- Tariffs are platform-controlled and versioned through audit. Rewards cannot change them.
- Ledgers and audit trails are immutable, with request ids and actors.
- Sandbox tokens are labelled and blocked in production. There are no fake approvals or fake credits.
- Feature flags (`wallet`, `card_payments`, `external_meters`, `benefits`) allow each tenant to run only what it is licensed for.
- Tenant data is isolated (supports per-partner DPAs).

## 4. Actions before production
1. Legal opinion on the vending role and the wallet / stored-value question (CBN). **Blocking.**
2. Choose the vending route:
   - STS vending partner and KMC for Zieon meters, **or**
   - DisCo or aggregator agreements for external meters.
   **Blocking.**
3. NEMSA certification and SON MANCAP/SONCAP for the Zieon meter model. **Blocking for hardware.**
4. Confirm the jurisdiction (NERC vs state regulator) for each pilot site, plus permits for estate generation.
5. NDPA programme: privacy notice, DPAs with partners, DPIA, breach runbook, and NDPC registration if applicable.
6. NCC type approval for the meter's communications module, if applicable.
