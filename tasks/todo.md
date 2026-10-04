# Implementation backlog — 04.10.2026

**План, не выполненная реализация.** 51 ticket. Все unchecked. Пути ниже — предлагаемые будущие files/components, не утверждение об их наличии. Scope: один account, одна сеть, без leverage/bridge; main plan [plan.md](plan.md). Один ticket ориентировочно 0.5–2 engineer-days; если выход за пределы — разбить по acceptance artifact. Dependencies обязательны для dependent work, независимые исследования можно выполнять отдельно.

**Обновление реализации:** готов первый research контур, см. [Milestone 1](../docs/implementation-m1.md). P1-01: pinned installation/offline verification выполнены; P1-02/04/05 и P2-04 имеют частичную реализацию (contracts, usage/call cap, portfolio adapter, native-engine probe). Полные tickets остаются unchecked: реальные LLM outputs, полная billing/schema/PIT/ledger семантика и corporate-action acceptance ещё не выполнены. Это не завершение P0–P8 или live gate.

## Phase 0 — Verification

Архитектурное уточнение 05.10.2026 и Go migration tickets S1–S6 вынесены в
[service-migration.md](service-migration.md). Новый backend пишем на Go,
Python сохраняет upstream research/evaluation. Исходные acceptance gates ниже
остаются обязательными; наличие новой архитектуры их не завершает.
Исторические Python-пути для scheduler/jobs/control/ledger заменяются на
Go module `go/` при реализации; Python-пути research/evaluation сохраняются
с учётом фактической текущей структуры.

### [ ] P0-01 — Account-specific eligibility dossier

- **Goal:** выбрать разрешённые research и execution paths.
- **Context:** EU retail не равен direct qualified issuer client; конкретная страна UNKNOWN.
- **Files/components:** `docs/eligibility.md`, `docs/provider-access.csv`.
- **Acceptance:** country/category/account/API/asset/redemption conditions, terms date/hash и доказательство по xStocks distributor/xChange/Ondo/broker; unresolved явно CLOSED для live; Belarus appendix отдельно.
- **Tests:** каждая разрешённая capability подтверждена account-specific evidence; documentation-only не помечена approved.
- **Dependencies:** нет; окончательный live результат требует конкретной страны и onboarding.

### [ ] P0-02 — Pin upstream and license manifest

- **Goal:** воспроизводимый состав reuse.
- **Context:** TA v0.6.0 свежий; alternatives/licensing неоднородны.
- **Files/components:** `docs/dependencies.md`, `research/upstream-manifest.json`.
- **Acceptance:** immutable SHAs TA/Nautilus/SDK; release date, license, patch policy, unresolved compatibility; нет proprietary ATLAS prompts в поставке.
- **Tests:** referenced SHAs/files существуют; license identifiers/conditions сверены с LICENSE.
- **Dependencies:** нет.

### [ ] P0-03 — Data rights and timestamp sample

- **Goal:** выбрать допустимые источники data/news/actions.
- **Context:** бесплатный demo feed не доказанный production PIT dataset.
- **Files/components:** `docs/data-contracts.md`, `tests/fixtures/data_samples/`.
- **Acceptance:** sample timestamps/revisions/coverage/retention rights; real quotes vendors либо UNKNOWN budget; SEC/FRED access policy.
- **Tests:** sample различает event/availability/ingestion; historic amendments и news corrections представлены.
- **Dependencies:** нет.

### [ ] P0-04 — Asset registry evidence

- **Goal:** избежать ticker/mint/ratio путаницы.
- **Context:** wrapper legal terms и Scaled UI различны.
- **Files/components:** `data/manifests/assets.json`, `docs/assets.md`.
- **Acceptance:** все 8 underlyings с issuer/chain/mint/decimals/ratio/terms source; unavailable instrument явно отсутствует, не guessed address.
- **Tests:** duplicate identity/counterfeit mint/ratio unit checks; registry date/version.
- **Dependencies:** P0-01; public evidence можно собрать заранее.

### [ ] P0-05 — Quote-study protocol

- **Goal:** получить сравнимые cost observations.
- **Context:** ни одна U-cell main plan пока не quote evidence.
- **Files/components:** `experiments/quote-study/protocol.md`, `contracts/quote-observation.json`.
- **Acceptance:** 8×5 sizes, buy/sell/session matrix, pilot sizes, soft/hard distinction, TTL/reference/fees fields, provider load limits и >=20 business days schedule.
- **Tests:** synthetic observation не проходит как executable; missing/ref-stale labels корректны.
- **Dependencies:** P0-01, P0-04.

### [ ] P0-06 — Register economics and decision gates

- **Goal:** зафиксировать criterion до просмотра результатов.
- **Context:** success = net risk-adjusted excess, не число сделок.
- **Files/components:** `experiments/protocol.md`, `docs/budgets.md`.
- **Acceptance:** primary benchmark/base currency, cost buckets, loss/spend caps, CI/power approach, change policy, go/hold/stop owner decisions.
- **Tests:** пример $1k cashflow/budget воспроизводим; benchmark investability явно отмечена.
- **Dependencies:** P0-03, P0-05.

## Phase 1 — Local baseline

### [ ] P1-01 — Pinned TradingAgents offline baseline

- **Goal:** первый coding artifact без wallet.
- **Context:** release main не плавающая dependency.
- **Files/components:** `python/pyproject.toml`, lockfile, `experiments/baseline/`, upstream fork reference.
- **Acceptance:** setup documented; installed version matches SHA; upstream checks recorded; single-symbol fixture run archived with hashes; no execution credentials.
- **Tests:** fresh-environment offline replay; network disabled fixtures; upstream test outcome report.
- **Dependencies:** P0-02; P0-03 sample.

### [ ] P1-02 — Typed intent and snapshot contracts

- **Goal:** заменить ambiguous sizing/action outputs.
- **Context:** upstream typed PM не готовый executable order.
- **Files/components:** `contracts/intent.json`, `contracts/portfolio.json`, `python/trader/contracts.py`, fixtures.
- **Acceptance:** five actions, target/delta/units, horizon/expiry/evidence/scenarios; no NaN/negative/inconsistent amount; fallback free text rejected.
- **Tests:** invalid action, probability sums, inconsistent units, stale snapshot, SELL without instrument.
- **Dependencies:** P1-01, P0-04.

### [ ] P1-03 — Immutable snapshot tool boundary

- **Goal:** tools возвращают только разрешённый frozen context.
- **Context:** upstream day cutoff недостаточен для intraday.
- **Files/components:** `python/trader/snapshots.py`, `python/trader/tools.py`, `data/manifests/`.
- **Acceptance:** as_of timestamp, source/available_at/hash; no agent live vendor fallback during replay; macro reused by snapshot.
- **Tests:** late news refused, content hash mismatch refused, offline tool replay complete.
- **Dependencies:** P1-02, P0-03.

### [ ] P1-04 — Usage and billing instrumentation

- **Goal:** измерить full decision p95 cost.
- **Context:** aggregate tokens без rates/cache/retry неполны.
- **Files/components:** `python/trader/usage.py`, `contracts/usage.json`, `experiments/baseline/costs/`.
- **Acceptance:** model/role/rate date/input/cache/output/reasoning/retry/tool cost; cap interrupts new calls; provider-supported schema model identified.
- **Tests:** cache subset not double-counted; retry billed; missing usage flagged; cap enforced.
- **Dependencies:** P1-01, P0-06.

### [ ] P1-05 — Portfolio-aware batch allocation adapter

- **Goal:** несколько symbol proposals учитывают один cash ledger.
- **Context:** static single-ticker portfolio input не portfolio optimizer.
- **Files/components:** `python/trader/allocation.py`, `python/trader/agents_adapter.py`.
- **Acceptance:** marked exposure/pending/reserved balances передаются PM; one target batch; basis separated from forward utility; unsupported output abstains.
- **Tests:** five proposals cannot each consume same cash; reduce/hold semantics; entry-price anchoring counterexample.
- **Dependencies:** P1-02, P1-03.

### [ ] P1-06 — Baseline replay and variance report

- **Goal:** измерить runtime correctness/cost до расширения.
- **Context:** temperature zero не гарантирует повторяемость.
- **Files/components:** `experiments/baseline/report.md`, `python/trader/evaluation/replay.py`.
- **Acceptance:** fixed fixture corpus, bounded repeat sample, valid output/evidence/tool rounds/latency/cost report; failure reasons preserved.
- **Tests:** each run reconstructable; invalid intent never creates order; report separates synthetic/actual billed calls.
- **Dependencies:** P1-03, P1-04, P1-05.

## Phase 2 — Historical evaluator

### [ ] P2-01 — PostgreSQL event ledger and migrations

- **Goal:** authoritative cash/intent/event identifiers.
- **Context:** mutable Markdown не accounting ledger.
- **Files/components:** `db/migrations/001_ledger.sql`, `python/trader/store.py`, `contracts/events.json`.
- **Acceptance:** events/positions/cashflows/intents/orders/reservations, unique idempotency keys, decimal precision, migration/rollback/backup policy.
- **Tests:** conservation, duplicate event rejection, transactional rollback, migration on clean database.
- **Dependencies:** P1-02.

### [ ] P2-02 — SEC acceptance and vintage ingestion

- **Goal:** point-in-time fundamentals/macro.
- **Context:** filed date/revised latest series недостаточны.
- **Files/components:** `python/trader/ingest/sec.py`, `fred.py`, `data/manifests/`.
- **Acceptance:** accession acceptance/availability/revision; ALFRED vintage and release timing; fair-access cache/backoff.
- **Tests:** afternoon filing unavailable in morning; amendment cannot alter earlier snapshot; vintage fixture.
- **Dependencies:** P1-03, P0-03.

### [ ] P2-03 — Prices, news and historical universe archive

- **Goal:** исключить survivorship и future adjustment.
- **Context:** latest Yahoo adjusted bars и current index members недостаточны.
- **Files/components:** `python/trader/ingest/market.py`, `news.py`, `universe.py`, raw object manifests.
- **Acceptance:** raw bars/actions, first-seen/corrections, listings/delistings; missing licensed history explicitly blocks affected claims.
- **Tests:** delisted security retained; future split doesn't rescale historic cash fill; news correction cutoff.
- **Dependencies:** P0-03, P1-03.

### [ ] P2-04 — Nautilus compatibility spike

- **Goal:** подтвердить reuse engine до большого adapter.
- **Context:** token multiplier/actions не доказаны engine defaults.
- **Files/components:** `python/trader/evaluation/engine.py`, `tests/fixtures/actions/`, `docs/adr/evaluator.md`.
- **Acceptance:** pinned engine creates chronological two-asset NAV with cash/actions; supported gap/custom adapter scope recorded; fallback decision if failed.
- **Tests:** known roundtrip, dividend/split/multiplier, fractional decimals and fee currency.
- **Dependencies:** P0-02, P2-01.

### [ ] P2-05 — Next-executable-fill portfolio replay

- **Goal:** заменить ticker-first rating evaluation.
- **Context:** inference completion предшествует fill.
- **Files/components:** `python/trader/evaluation/replay.py`, `fills.py`.
- **Acceptance:** global timeline, latency/calendar, orders/partial/cancel/expiry/halts, dynamic portfolio snapshot; no same-close hindsight fill.
- **Tests:** simultaneous symbols/cash constraint, delayed decision, holiday/DST, partial fill.
- **Dependencies:** P2-04, P2-02, P2-03.

### [ ] P2-06 — Deterministic and investment benchmarks

- **Goal:** сопоставить сложный AI с простыми стратегиями.
- **Context:** nominal return не alpha.
- **Files/components:** `python/trader/evaluation/benchmarks.py`, `reports.py`.
- **Acceptance:** cash/B&H/equal-weight/trend; accessible vs theoretical flags; same base currency and funding/cost model.
- **Tests:** benchmark NAV fixtures, net dividends, rebalancing fees, inactive capital.
- **Dependencies:** P2-05, P0-06.

### [ ] P2-07 — Leakage and statistical protocol audit

- **Goal:** честно классифицировать retrospective evidence.
- **Context:** LLM weights могут содержать test outcomes.
- **Files/components:** `docs/leakage-register.md`, `python/trader/evaluation/statistics.py`, experiment split manifest.
- **Acceptance:** purge/embargo by horizon, frozen splits, model cutoff unknown label, blocked CI/multiple-testing plan, no clean-OOS claim без evidence.
- **Tests:** future observation/lesson negative controls; overlaps removed; synthetic correlated sample CI sanity.
- **Dependencies:** P2-05, P2-06.

## Phase 3 — Costs and quotes

### [ ] P3-01 — Read-only quote adapters

- **Goal:** собирать permitted quotes без подписи.
- **Context:** current Swap v2/issuer schemas нужно version pin.
- **Files/components:** `go/internal/venues/quote/`, generated API clients, recorded fixtures.
- **Acceptance:** one approved venue first; quote kind/TTL/fee/net output/limit/cash unit; other adapters only after own access evidence.
- **Tests:** current schema contract, stale/soft/hard/minimum/429/partial-response fixtures.
- **Dependencies:** P0-01, P0-04, P0-05.

### [ ] P3-02 — Execute quote-study collection

- **Goal:** заполнить U matrix measured observations.
- **Context:** public schedule не actual spread/size impact.
- **Files/components:** `experiments/quote-study/`, quote SQL table, collection job.
- **Acceptance:** coverage/rejection по8×5×session за >=20 business days; pilot sizes; no unsupported cells invented; rate/reservation policy obeyed.
- **Tests:** clock/reference alignment, duplicates, outage/rejection retention, dataset manifest hash.
- **Dependencies:** P3-01.

### [ ] P3-03 — Share-equivalent routing comparison

- **Goal:** корректно сравнить wrappers/held SELL.
- **Context:** token amounts разных issuers не взаимозаменяемы.
- **Files/components:** `go/internal/venues/normalize/`, `python/trader/evaluation/quote_compare.py`.
- **Acceptance:** ratio/decimals/reference normalization; sell only held instrument; chain/settlement/issuer constraints precede cost ranking.
- **Tests:** wrong-mint best-price trap, changed multiplier, fee embedded, USDon≠USDC.
- **Dependencies:** P3-01, P0-04.

### [ ] P3-04 — Fee/impact/failure cost replay

- **Goal:** evaluator uses full economic costs.
- **Context:** nominal 0-fee claims hide spread/gas.
- **Files/components:** `python/trader/evaluation/costs.py`, `go/internal/venues/costs/`.
- **Acceptance:** measured quote outputs + explicit fees, failed tx/priority/FX/issuer drag, rent locked separately, sensitivity when historical quotes absent.
- **Tests:** no double-counting; net-output fixture, high-priority fee, depeg, refunded rent.
- **Dependencies:** P2-05, P3-02, P3-03.

### [ ] P3-05 — Cheap scorer with budgeted candidate selection

- **Goal:** не анализировать500 equities full debate ежедневно.
- **Context:** universe prefilter, не замена AI trade decision.
- **Files/components:** existing Qlib workflow/config adapter либо existing TA indicator-ranking adapter, `python/trader/universe/scorer.py`, `config/universe.yaml`.
- **Acceptance:** reused feature/ranking implementation, no new screener framework; permitted assets, PIT indicators/catalysts/quote costs, configurable top-K, event reviews for holdings; feature parameters frozen.
- **Tests:** inaccessible/stale asset excluded; holdings event prioritized; max daily analyses respected.
- **Dependencies:** P2-03, P3-02, P1-04.

### [ ] P3-06 — Broker/token/OPEX economic report

- **Goal:** выбрать cheapest viable path либо stop.
- **Context:** $1k fixed-cost hurdle велик.
- **Files/components:** `experiments/economics/report.md`, `cashflow.csv`.
- **Acceptance:** five notionals/eight targets, actual unknown coverage, one full fiat cycle, OPEX scenarios, broker limitations/PRIIPs, capital capacity unknown.
- **Tests:** cashflow math, rates units/bps, compare same underlying/session/currency; no invented expected alpha.
- **Dependencies:** P3-04, P3-05, P2-06.

## Phase 4 — Memory/agent evaluation

### [ ] P4-01 — SQL decision and outcome memory

- **Goal:** durable PIT memories без vectors.
- **Context:** intraday decisions не ticker/date unique.
- **Files/components:** `db/migrations/002_memory.sql`, `python/trader/memory/store.py`.
- **Acceptance:** decision/outcome/lesson IDs; created/available/label_end; immutable original records; versioned correction events.
- **Tests:** multiple same-day decisions; no lesson before availability; replay independent of upstream rotation.
- **Dependencies:** P2-01, P2-05.

### [ ] P4-02 — Horizon-aware actual and shadow outcome labels

- **Goal:** не считать5-day raw return реальным P&L.
- **Context:** hours/months, partial exits, counterfactuals различны.
- **Files/components:** `python/trader/memory/outcomes.py`.
- **Acceptance:** executed net cashflow, open horizon marks, benchmark return; separate shadow rejection label; execution vs signal failure.
- **Tests:** month thesis not settled after5days, partial exit, no future benchmark labels, shadow not actual profit.
- **Dependencies:** P4-01, P3-04.

### [ ] P4-03 — Bounded factual retrieval and reflection

- **Goal:** полезный auditably sourced context.
- **Context:** reflection может усилить luck/anchoring.
- **Files/components:** `python/trader/memory/retrieval.py`, `reflection.py`.
- **Acceptance:** source IDs/PIT filters/context cap; ticker/sector/regime retrieval; lesson confidence/decay, no factual ledger rewrite.
- **Tests:** poisoned lesson doesn't become command, future lesson excluded, context budget, entry-price anchoring examples.
- **Dependencies:** P4-02, P1-04.

### [ ] P4-04 — Agent/memory paired ablations

- **Goal:** измерить incremental benefit и расходы.
- **Context:** complexity не доказательство alpha.
- **Files/components:** `experiments/ablations/`, `python/trader/evaluation/ablations.py`.
- **Acceptance:** single-agent/no-memory/factual/reflection/full graph; same inputs/cost envelope; results and all variants retained; semantic retrieval только отдельный challenger.
- **Tests:** shared dataset hashes/splits; prompt variations registered; paired CI and billed cost.
- **Dependencies:** P4-03, P2-07, P3-06.

### [ ] P4-05 — Freeze strategy and forward protocol

- **Goal:** выбрать simpler winner и зафиксировать prospective experiment.
- **Context:** ретроспективная optimization не clean evidence.
- **Files/components:** `experiments/forward/protocol.md`, `config/frozen-strategy.yaml`.
- **Acceptance:** exact model IDs/prompts/risk/memory/scorer hashes; primary comparison, change/challenger policy и insufficient-power outcome.
- **Tests:** config tampering starts new run; no hindsight variant removal.
- **Dependencies:** P4-04, P0-06.

## Phase 5 — Paper operation

### [ ] P5-01 — SQL jobs, leasing and outbox

- **Goal:** dependable event scheduling без Redis/Kafka.
- **Context:** duplicate triggers не должны создавать двойные trades.
- **Files/components:** `db/migrations/003_jobs.sql`, `python/trader/jobs.py`.
- **Acceptance:** idempotent event IDs, lease expiration, attempt history, budgets and one account writer.
- **Tests:** two workers lease once; crash/restart; expired lease recovery, duplicate catalyst.
- **Dependencies:** P2-01, P4-05.

### [ ] P5-02 — Position/thesis review lifecycle

- **Goal:** автономные holds/adds/reductions по horizon/event.
- **Context:** без forced daily exit и basis anchoring.
- **Files/components:** `python/trader/positions.py`, `review.py`.
- **Acceptance:** thesis state отдельно order state; next_review/invalidation; global portfolio version; pending orders visible.
- **Tests:** review event once; partial opening; improved forward thesis despite loss; holding after unchanged catalyst.
- **Dependencies:** P1-05, P5-01.

### [ ] P5-03 — Deterministic risk and reservations

- **Goal:** account limits перед simulated execution.
- **Context:** AI decides signal, kernel only safety.
- **Files/components:** `go/internal/risk/`, SQL reservation transactions, `config/risk.yaml`.
- **Acceptance:** caps/modes/units/freshness/eligibility/fees/global balances; no action inversion; cash and token reservation.
- **Tests:** concurrent buys, excessive correlated exposure, stale version, reduce-only/unknown balance, no leverage.
- **Dependencies:** P2-01, P1-02, P3-03.

### [ ] P5-04 — Forward paper venue and ledger reconciliation

- **Goal:** prospective feasible fills, not rating-only results.
- **Context:** quotes recorded after decision completion.
- **Files/components:** `go/internal/venues/paper/`, `python/trader/evaluation/forward.py`.
- **Acceptance:** real-time observed quote/latency/rejection, NAV including idle capital and OPEX, benchmark same clock; run begins after freeze.
- **Tests:** no stale/soft-as-guaranteed fill; outage retained; amount conservation, next-fill timestamp.
- **Dependencies:** P5-02, P5-03, P3-04.

### [ ] P5-05 — Monitoring, backup and recovery

- **Goal:** наблюдаемая эксплуатация и восстановление.
- **Context:** paper выявляет operational problems до funds.
- **Files/components:** `python/trader/observability.py`, `go/internal/telemetry/`, `docs/runbooks/paper.md`, `deploy/`.
- **Acceptance:** trace chain, budgets/reconciliation/stale alerts, backups, successful restore and secret redaction; EU data-processing check recorded.
- **Tests:** injected feed outage/NAV mismatch/spend spike; restored run resumes once; no credentials in logs.
- **Dependencies:** P5-04.

### [ ] P5-06 — Forward evidence and power review

- **Goal:** принять go/hold/stop по пререгистрированным данным.
- **Context:** 8–12 weeks reliability не автоматически alpha proof.
- **Files/components:** `experiments/forward/reports/`, `docs/gates/G5.md`.
- **Acceptance:** monthly frozen gross/execution-net/operating-net metrics, block CI/power/regime coverage, no dropped days; explicit adequate/inconclusive/negative outcome.
- **Tests:** recompute from ledger; benchmark/cost invariants; decision made without test retuning.
- **Dependencies:** P5-05, observed prospective sample.

## Phase 6 — Execution security

### [ ] P6-01 — One permitted venue build/submit/status adapter

- **Goal:** minimal execution path без universal router.
- **Context:** start with one approved chain/venue and real versioned docs.
- **Files/components:** `go/internal/venues/jupiter/` либо иной approved adapter, `go/cmd/executor/`.
- **Acceptance:** quote→transaction→status semantics, signed payload hash, TTL/blockhash, no blind retry; capability table.
- **Tests:** API recorded contract, expired quote, unknown submission status, minimum/fee error.
- **Dependencies:** P3-01, P5-03, G0/G3 access.

### [ ] P6-02 — Independent transaction decoder/policy verifier

- **Goal:** reject malicious built transaction до signing.
- **Context:** quote provider untrusted; Token-2022/lookup tables critical.
- **Files/components:** `go/internal/verify/`, adversarial transaction fixtures.
- **Acceptance:** full resolved account/program/instruction decode, exact source/destination/mints/min-output/fees; unknown/delegate/authority/bridge rejected.
- **Tests:** hidden transfer, fake ATA, changed recipient, unknown program, lookup-table mutation, token extension, fee drain.
- **Dependencies:** P6-01, P0-04.

### [ ] P6-03 — Pure Ed25519/KMS compatibility spike

- **Goal:** подтвердить concrete signing algorithm/region.
- **Context:** KMS availability не trading policy.
- **Files/components:** `signer/spike/`, `docs/adr/signer.md`.
- **Acceptance:** known-vector Solana message verification, RAW pure Ed25519 algorithm, selected EU region/provider access, latency/cost and recovery constraints.
- **Tests:** wrong prehash variant fails; signature verifies with maintained SDK; no private key export/log.
- **Dependencies:** P0-02, P6-02.

### [ ] P6-04 — Isolated signer and signed risk config

- **Goal:** execution host не имеет unrestricted signing.
- **Context:** verifier independent trust boundary.
- **Files/components:** `signer/service/`, `signer/policy/`, `deploy/signer/`.
- **Acceptance:** authenticated requests, policy/intent/transaction hash verification, nonce/expiry/caps, owner config, separate IAM role; Python/Go caller no KMS Sign.
- **Tests:** unauthorized caller, arbitrary payload, stale config, replay, cap bypass, compromised executor scenario.
- **Dependencies:** P6-02, P6-03, threat-model review.

### [ ] P6-05 — Submit idempotency and finality reconciliation

- **Goal:** exactly-once economic effect where chain semantics allow.
- **Context:** API timeout ≠ failed transaction.
- **Files/components:** `go/internal/orders/`, `go/internal/reconcile/`, SQL order events.
- **Acceptance:** submitted/unknown/confirmed/finalized/reconciled states; independent RPC balances; before resend status and expiry checks, reserved balances remain conservative.
- **Tests:** timeout then late finalization, node disagreement, duplicate callback, expired blockhash/resubmit, partial portfolio completion.
- **Dependencies:** P6-01, P5-03.

### [ ] P6-06 — Cold/float refill and emergency recovery runbook

- **Goal:** funds outside bot remain outside compromise radius.
- **Context:** raw-key full compromise can drain float.
- **Files/components:** `docs/runbooks/wallet.md`, `docs/runbooks/incident.md`.
- **Acceptance:** owner-only funding/withdrawal, float/gas cap, independent killswitch, KMS/IAM outage and pending-tx recovery; non-exportable key limitations explicit.
- **Tests:** tabletop plus sandbox restore, no bot auto-refill, independent shutdown, balance/ledger restored.
- **Dependencies:** P6-04, P6-05.

### [ ] P6-07 — Adversarial review and gate report

- **Goal:** complete safety evidence before mainnet funds.
- **Context:** good backtest cannot waive signing risk.
- **Files/components:** `docs/gates/G6.md`, security corpus/tests, dependency manifest.
- **Acceptance:** all critical TM paths tested; no unresolved arbitrary signing/double-spend/accounting issue; terms refreshed; independent verifier boundary substantiated.
- **Tests:** prompt injection→intent→signer chain, compromised quote builder, unauthorized IAM, dependency/restore scenario.
- **Dependencies:** P6-02…06, P5-05.

### [ ] P6-08 — Optional second venue adapter behind capability gate

- **Goal:** add xChange/Ondo only when measurable benefit justified.
- **Context:** not prerequisite for one-path pilot; no speculative API implementation.
- **Files/components:** `go/internal/venues/xchange/` или `ondo/`, generated clients/fixtures.
- **Acceptance:** approved access/minimum/settlement asset; normalized quotes, decoded transaction policy and reconciliation; if unavailable ticket deferred without blocking pilot.
- **Tests:** hard/soft, attestation expiry, issuer limits/halts, USDon residual balance, held-wrapper exit.
- **Dependencies:** own P0-01/P3 evidence, P6-07.

## Phase 7 — Bounded live

### [ ] P7-01 — Approve risk envelope and fund micro canary

- **Goal:** owner-controlled first mainnet validation.
- **Context:** planned cap/minimum must agree; no human approval per ordinary trade afterward.
- **Files/components:** `docs/gates/G7-start.md`, `config/live-envelope.yaml`, owner wallet action outside code.
- **Acceptance:** G5/G6 passed, precise total/float/order/loss caps, measured available small size; owner mode activation and funding recorded, no seed in repo/chat.
- **Tests:** allowed payload/exit checked, killswitch responsive, pre/post balances match.
- **Dependencies:** P5-06 passing gate, P6-07; P6-08 only for that chosen route.

### [ ] P7-02 — Canary settlement and controlled exit report

- **Goal:** проверить real path и return-to-cold ability.
- **Context:** testnet не подтверждает mainnet issuer liquidity.
- **Files/components:** `experiments/live-canary/report.md`, live order/fee ledger.
- **Acceptance:** confirmed/finalized fill, actual net output/fee/multiplier, controlled permitted exit, no unknown residual funds; stop on mismatch.
- **Tests:** independent RPC and owner wallet balances, actual bps vs quote, no duplicate economic effect.
- **Dependencies:** P7-01.

### [ ] P7-03 — Bounded $1k total-capital pilot operation

- **Goal:** autonomous operation внутри small float envelope.
- **Context:** max $250–500 float, idle capital benchmark included.
- **Files/components:** `experiments/live-pilot/`, operational config and reports.
- **Acceptance:** frozen strategy, actual costs/OPEX, incidents/rejections retained, no auto-refill/scale, daily marks plus thesis reviews.
- **Tests:** cap/reduce-only/freeze events; reconciliation and spend monitors; replay from audit bundle.
- **Dependencies:** P7-02.

### [ ] P7-04 — Live-vs-paper and stop/continue review

- **Goal:** отделить implementation slippage от alpha claim.
- **Context:** few trades не statistical success.
- **Files/components:** `docs/gates/G7-review.md`, live comparative reports.
- **Acceptance:** fill/quote drift, net excess CI/power, fee distribution, drawdown, incident root causes; next action stop/paper/continue clearly recorded.
- **Tests:** recomputed NAV including cold idle reserve; no excluded failed tx or monthly cost.
- **Dependencies:** P7-03 and adequate observed sample.

## Phase 8 — Capacity/termination

### [ ] P8-01 — Capital and capacity cost curves

- **Goal:** проверить next-size economics без leverage.
- **Context:** scaling may dilute OPEX but worsen market impact.
- **Files/components:** `experiments/capacity/`, quote-size report.
- **Acceptance:** tested size buckets, all-in cost/availability/issuer concentration, capital breakeven only with measured excess uncertainty; no promised return.
- **Tests:** non-linear slippage, maximum venue limits, liquidity outage, cash reserve requirements.
- **Dependencies:** P7-04, refreshed P3 study.

### [ ] P8-02 — Independent evidence/security review

- **Goal:** проверить scale case вне разработки.
- **Context:** author self-report недостаточен.
- **Files/components:** `docs/reviews/scale.md`, evidence bundle.
- **Acceptance:** reviewer can reconstruct results/fees/terms/controls; unresolved high-risk findings block scale; licenses and provider processing regions refreshed.
- **Tests:** independent rerun accounting/statistical reports; boundary and recovery review at new cap.
- **Dependencies:** P8-01.

### [ ] P8-03 — Scale, simplify or terminate decision

- **Goal:** выполнить objective-oriented итог, не спасать исходную гипотезу.
- **Context:** no net alpha → strategy not successful.
- **Files/components:** `docs/gates/G8.md`, archived run manifest, owner next-cap config при GO.
- **Acceptance:** explicit decision and evidence; positive adequate net/risk/capacity case only allows bounded scale; otherwise freeze/return funds/simplify broker or deterministic baseline.
- **Tests:** terminal run immutable; returns/recovery reconciled; next config can't silently change previous evidence.
- **Dependencies:** P8-02.

## Checklist состояния

- [x] Прочитан запрос, принят EU основной сценарий.
- [x] Проверены первичные документы и pinned upstream source snapshots.
- [x] Составлены 37-section architecture/research plan и малые tickets.
- [x] Unknown quotes, legal eligibility и alpha отделены от доказанных фактов.
- [ ] P0–P8 implementation/experiments выполнены — **нет, требуют следующих этапов**.
- [ ] Автономный live допущен — **нет**.
- [ ] Net risk-adjusted alpha доказана — **нет**.
