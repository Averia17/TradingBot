# Реестр первичных источников и границы доказательств

Этот документ — архив первоначального исследования. Новая реализация и выполненные тесты описаны в [Milestone 1](../docs/implementation-m1.md) и [актуальном выборе репозиториев](implementation-selection.md).

**Дата проверки: 04.10.2026.** Запрос выполнен как исследование и проектирование без implementation trading code. Primary sources: official repository/source/API, issuer documentation, fee schedules, papers. GitHub issue — сообщение автора issue, не автоматически подтверждённый bug; README performance — author claim. API schema — возможность интеграции, не подтверждение account eligibility. Published fee — не executable all-in quote.

Main deliverables: [план](../tasks/plan.md), [backlog](../tasks/todo.md), [threat model](../TradingBot-threat-model.md). Paths относительно этого файла. Локальные clones скачаны только для чтения, их код не устанавливался и не запускался; они содержат собственные `.git` directories. Их не следует рекурсивно добавлять в будущий application repo. Большие datasets, keys и реальные funds не использовались.

## 1. Pinned repository snapshots

| Project | Inspected commit | Activity snapshot | License / caveat |
|---|---|---|---|
| [TradingAgents](https://github.com/TauricResearch/TradingAgents) | `1394a3f72aa4393e1a98f51b382434c4b4c2d972` | v0.6.0; pushed 03.10.2026; 109755 stars / 21096 forks | Apache-2.0 |
| [FinAgent](https://github.com/DVampire/FinAgent) | `17248a0b8b729ee3e093e30bb7bea7f52181f363` | pushed 31.08.2024; 75/27 | MIT |
| [FinMem](https://github.com/pipiku915/FinMem-LLM-StockTrading) | `be814aa47970de9bf2fdd6a1d5a60ae5cf361b46` | pushed 18.08.2024; 962/195 | MIT |
| [FinRL-X / FinRL-Trading](https://github.com/AI4Finance-Foundation/FinRL-Trading) | `4409abe925c904e570be78ebfb5e77ac3491dff8` | pushed 18.09.2026; 3780/1107 | Apache-2.0 |
| [AI Hedge Fund](https://github.com/virattt/ai-hedge-fund) | `78b779c1389e2d1452dc29606d2c4126d859b964` | pushed 02.10.2026; 63857/11223 | MIT |
| [ATLAS](https://github.com/chrisworsey55/atlas-gic) | `cf4349f15c9d68a67042f792973372f2e0231088` | pushed 22.09.2026; 2307/416 | LICENSE MIT для framework/docs/examples; production prompts proprietary, absent |

API snapshots: [tradingagents-github.json](tradingagents-github.json), [tradingagents-issues.json](tradingagents-issues.json), [ecosystem-github.json](ecosystem-github.json). API license `NOASSERTION` требует чтения LICENSE, не вывод «open source unrestricted».

Discovery-only, без полного source audit: [FinRL](https://github.com/AI4Finance-Foundation/FinRL), [FinWorld](https://github.com/DVampire/FinWorld), [TradingAgents-CN](https://github.com/hsliuping/TradingAgents-CN), [OpenAlice](https://github.com/TraderAlice/OpenAlice), [Predict/Raven](https://github.com/Alchemist-X/predict-raven). В API FinRL 16550/3537, FinWorld 133/27, CN 32146/6719, OpenAlice 7207/1134, Raven 75/29. Старые last-push даты — maintenance signal, не доказательство broken runtime. Star counts не означают verified community live deployments.

## 2. TradingAgents source evidence

Все source ссылки этого раздела immutable на `1394a3f…`. Локально тот же код в `research/TradingAgents`.

| Проверяемый вывод | Первичный anchor | Ограничение вывода |
|---|---|---|
| Latest stable v0.6.0 | [release](https://github.com/TauricResearch/TradingAgents/releases/tag/v0.6.0), saved GitHub API release metadata | Не прогноз отсутствия regression в свежем release |
| Two tiers, default model IDs, limits, 5-day settlement | [default_config.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/default_config.py#L93) | Defaults не выбранные/оплаченные нами модели; real calls UNKNOWN |
| Analyst parallel graph, private message state, later barrier/debate | [graph/setup.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/graph/setup.py), [analyst_execution.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/graph/analyst_execution.py) | Parallel analysts не independent account execution writers |
| Day-based propagation/checkpoint/portfolio context | [trading_graph.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/graph/trading_graph.py) | Intraday safety требует timestamp wrapper |
| Typed output, free-string position sizing | [schemas.py:180](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/agents/schemas.py#L180) | Typed recommendation ≠ fully typed executable intent |
| PM structured→text fallback and partial-profits prompt | [portfolio_manager.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/agents/managers/portfolio_manager.py) | Fallback research valid, execution must reject |
| Cash/positions/average entry snapshot | [portfolio.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/portfolio.py) | Нет нашего reservations/concentration/quote ledger |
| Ticker-first/date-second backtest | [backtest.py:160](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/backtest.py#L160) | Signal scoring useful; не full evolving portfolio evaluator |
| Durable file memory, per ticker/date dedup, as-of filtering/update | [memory/log.py:48](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/memory/log.py#L48), [retrieval:93](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/memory/log.py#L93) | No vector DB requirement; file locks не immutability |
| Fixed-window raw returns/benchmark and reflection | [settlement.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/memory/settlement.py), [reflection.py:36](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/memory/reflection.py#L36) | Not actual fills/net realized P&L; incremental benefit UNKNOWN |
| SEC facts filed day cutoff | [sec_edgar.py:176](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/dataflows/vendors/sec_edgar.py#L176) | Correct date filter improvement; exact acceptance time absent |
| FRED vintage params | [fred.py:204](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/dataflows/vendors/fred.py#L204) | Release time still separate |
| Yahoo adjusted history | [ohlcv.py:215](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/tradingagents/dataflows/vendors/yahoo/ohlcv.py#L215) | Future actions can change scale; do not infer measured strategy loss |
| LLM/tool usage counters | [cli/stats_handler.py](https://github.com/TauricResearch/TradingAgents/blob/1394a3f72aa4393e1a98f51b382434c4b4c2d972/cli/stats_handler.py) | No complete provider/model billing proof |

Selected issues/PR checked: [#805 model leakage](https://github.com/TauricResearch/TradingAgents/issues/805), [#1374 valuation](https://github.com/TauricResearch/TradingAgents/issues/1374), [#1390 structured provider behavior](https://github.com/TauricResearch/TradingAgents/issues/1390), [#1479 rotation retention](https://github.com/TauricResearch/TradingAgents/pull/1479), [#515 token budget](https://github.com/TauricResearch/TradingAgents/issues/515), [#750 cache](https://github.com/TauricResearch/TradingAgents/issues/750). Do not assume pending fix merged or cache-saving claims reproduced.

## 3. Alternative source evidence

- FinAgent [environment/trading.py:117](https://github.com/DVampire/FinAgent/blob/17248a0b8b729ee3e093e30bb7bea7f52181f363/finagent/environment/trading.py#L117) builds future slice; this is an **audit trigger**, not proof test predictions consume future values. [basic_memory.py](https://github.com/DVampire/FinAgent/blob/17248a0b8b729ee3e093e30bb7bea7f52181f363/finagent/memory/basic_memory.py) shows vector retrieval/error behavior. Reflection train/eval separation needs trace before reuse.
- FinMem [memorydb.py](https://github.com/pipiku915/FinMem-LLM-StockTrading/blob/be814aa47970de9bf2fdd6a1d5a60ae5cf361b46/puppy/memorydb.py) layers/decay/access feedback, [checkpoint load:420](https://github.com/pipiku915/FinMem-LLM-StockTrading/blob/be814aa47970de9bf2fdd6a1d5a60ae5cf361b46/puppy/memorydb.py#L420) pickle: never load untrusted checkpoint. Embeddings and added complexity require ablation.
- FinRL-X [ml_bucket_selection.py:86](https://github.com/AI4Finance-Foundation/FinRL-Trading/blob/4409abe925c904e570be78ebfb5e77ac3491dff8/src/strategies/ml_bucket_selection.py#L86) report-date mapping; [ml_strategy.py:72](https://github.com/AI4Finance-Foundation/FinRL-Trading/blob/4409abe925c904e570be78ebfb5e77ac3491dff8/src/strategies/ml_strategy.py#L72) TODO. Some code supports historical universe CSV; fallback/current-screener behavior cannot be treated universally PIT-safe.
- AI Hedge Fund [paper ledger](https://github.com/virattt/ai-hedge-fund/blob/78b779c1389e2d1452dc29606d2c4126d859b964/hedge_fund/paper/ledger.py), [paper broker](https://github.com/virattt/ai-hedge-fund/blob/78b779c1389e2d1452dc29606d2c4126d859b964/hedge_fund/brokers/paper.py), [risk limits](https://github.com/virattt/ai-hedge-fund/blob/78b779c1389e2d1452dc29606d2c4126d859b964/hedge_fund/risk/limits.py); repo describes educational/paper operation, not verified autonomous live account.
- ATLAS [LICENSE](https://github.com/chrisworsey55/atlas-gic/blob/cf4349f15c9d68a67042f792973372f2e0231088/LICENSE) expressly limits included artifacts; proprietary production prompts absent.
- Reuse candidates [NautilusTrader](https://github.com/nautechsystems/nautilus_trader), [reports source](https://github.com/nautechsystems/nautilus_trader/blob/develop/docs/concepts/reports.md), [Qlib](https://github.com/microsoft/qlib): source/doc discovery, **not installed/tested versions**. Exact pin/Token-2022/action compatibility deferred P0/P2.

## 4. Empirical evidence classification

| Source | Tier | What main plan uses | What it does not establish |
|---|---|---|---|
| [TradingAgents arXiv v1](https://arxiv.org/html/2412.20138v1) | Author backtest | Three-stock return/Sharpe/MDD tables; inconsistent period descriptions flagged | Independently reproduced, audited live, all-cost alpha |
| [FinAgent arXiv v2](https://arxiv.org/html/2402.18485v2) | Author backtest | TSLA ARR/SR/MDD; FinMem comparison | ARR is not total return; no full recurring OPEX proof |
| [FinMem arXiv v2](https://arxiv.org/html/2311.13743v2) | Author research | Memory architecture comparison; paper results remain author claims | Our portfolio/fees/horizons replication |
| [FinRL-X README](https://github.com/AI4Finance-Foundation/FinRL-Trading) | Author historic/paper claims | Oct2025–Mar2026 quoted paper metrics | Audited execution, current forward generalization |
| [ATLAS README](https://github.com/chrisworsey55/atlas-gic) | Author deployment claim | +22%/173 days labelled unverified | Costs, benchmark, independent equity alpha |
| [Raven README](https://github.com/Alchemist-X/predict-raven) | Author forward/paper ops record | Operational disclosure and shadow patterns | Verified equity live alpha; live Polymarket status was paused |
| Our independent replay / prospective / live | No result yet | Proposed experiments only | Any expected return or proven net alpha |

No qualified independent all-cost replication of the chosen TA version was located in this research. This is a search finding, not a universal assertion that none exists. Model weight knowledge leakage cannot be eliminated by historical retrieval filters alone.

## 5. xStocks / Backed sources

| Official source | Claim boundary |
|---|---|
| [Product legal overview](https://docs.xstocks.fi/docs/product-legal-overview) | Issuer/claim/backing/structure; not ownership of voting shares |
| [Service providers](https://assets.backed.fi/legal-documentation/service-providers) | Current general custodians/security agent; series final terms still required |
| [Restricted countries](https://assets.backed.fi/legal-documentation/restricted-countries) | Belarus non-serviceable, direct-client/distributor conditions; not account approval |
| [Market Flow](https://docs.xstocks.fi/docs/issuance-and-redemption/market-flow) | Published $5000 minimum/whitelisted workflow; not small retail access |
| [Atomic RFQ](https://docs.xstocks.fi/docs/issuance-and-redemption/atomic-rfq-xchange) | Variable spread, price includes margin, no additional protocol fee; no universal bps |
| [xChange API](https://docs.xstocks.fi/developers/xchange-atomic-rfq) | Soft/hard lifecycle, asset min/max config, signed payloads; our access UNKNOWN |
| [Developers](https://docs.xstocks.fi/developers) | Metadata/multiplier/PoR/action/chain interfaces; chain-by-asset registry authoritative |
| [FAQ](https://docs.xstocks.fi/docs/frequently-asked-questions) | General redemption/dividend/custody conditions, not all series-specific resolution |

Saved selected documentation: [xstocks-legal.md](xstocks-legal.md), [xstocks-faq.md](xstocks-faq.md). Supported multiple EVM/Solana implementations do not imply each token tradable on every chain or aggregator. Token contract authority/upgrade/freeze controls, decimals/extensions и asset terms должны проверяться по actual registry/address в P0/P6; здесь contract audit не проводился.

**Unknown:** authenticated API rate limits/SLAs for our account, current min/max each instrument, executable BUY/SELL prices all40 cells, hard-quote reservation terms, legal permitted DEX access, delisting/spinoff recovery specifics. Это experimental/onboarding gates.

## 6. Ondo official sources

| Source | Evidence |
|---|---|
| [Eligibility](https://docs.ondo.finance/ondo-stocks/eligibility) | EEA direct professional/qualified requirement; residency/location restrictions |
| [Secondary restrictions](https://docs.ondo.finance/ondo-stocks/secondary-market-restrictions) | Secondary prohibited-person conditions; not blanket retail approval |
| [Trust/transparency](https://docs.ondo.finance/ondo-stocks/trust-and-transparency) | BVI issuer/collateral/governance/security-verification claims; legal docs determine recovery |
| [Technical](https://docs.ondo.finance/ondo-stocks/technical) | Chains/token architecture; bridges outside MVP |
| [Pricing](https://docs.ondo.finance/ondo-stocks/token-and-quote-pricing) | Share/total-return ratios, quote semantics; no universal executed spread |
| [Fees/taxes](https://docs.ondo.finance/ondo-stocks/fees-and-taxes) | Spread/gas/structure-level withholding; user's tax country unresolved |
| [Investing/redeeming](https://docs.ondo.finance/ondo-stocks/investing-and-redeeming) | USDon/whitelist/swapper workflow; USDC conversion conditional |
| [Market hours](https://docs.ondo.finance/ondo-stocks/market-hours-and-trading-availability) | Primary sessions/halts/pauses |
| [Off-hours](https://docs.ondo.finance/ondo-stocks/off-hours-trading) | Selected weekend/holiday assets, conservative caps; no universal24/7 guarantee |
| [Corporate actions](https://docs.ondo.finance/ondo-stocks/corporate-actions) | Dividends/actions/reinvestment behavior; validate instrument-specific handling |
| [API overview](https://docs.ondo.finance/api-reference/overview) | REST/OpenAPI and gRPC; access/limits require onboarding |
| [Mint/redeem attestation](https://docs.ondo.finance/api-reference/attestations/request-a-mint-or-redeem-attestation) | Attestation validity/settlement specification; our executable quote UNKNOWN |

Selected markdown copies `ondo-*.md` and [index](ondo-index.txt) saved locally. API provider documentation does not prove direct retail eligibility. Rate limits, exact minimum, live quote spreads and after-hours capacity **UNKNOWN** for our account.

## 7. Other protocols/distributors and brokers

- [Dinari fees](https://docs.dinari.com/docs/fees), [quickstart/Go SDK](https://docs.dinari.com/docs/quickstart), [restrictions](https://docs.dinari.com/docs/restrictions), [KYC](https://docs.dinari.com/docs/managing-kyc): enterprise/API economics and access conditions distinct from partner retail. No independent $1k direct API suitability established.
- [Securitize May2026 onchain stock announcement](https://investors.securitize.io/news/news-details/2026/Securitize-Jump-Trading-Group-and-Jupiter-Launch-Fully-Onchain-Regulated-Trading-for-Tokenized-Equities/default.aspx), [EU TSS approval](https://investors.securitize.io/news/news-details/2025/Securitize-Wins-Full-EU-Regulatory-Approval-and-Selects-Avalanche-for-Initial-Deployment-of-European-Trading-Settlement-System-11-26-2025/default.aspx), [stock platform](https://stocks-evm.securitize.io/): evidence of distinct securities route, not confirmation of eight-target API/minimum/retail access.
- [Alpaca ITN authorized-participant guide](https://docs.alpaca.markets/us/docs/tokenization-guide-for-authorized-participant): AP inventory network, not presumed retail bot account.
- [Kraken xStocks FAQ](https://support.kraken.com/articles/xstocks-faq), [Pro fee schedule](https://www.kraken.com/pro/xstocks), [regulation/countries](https://support.kraken.com/articles/where-is-kraken-licensed-or-regulated): fees/product/country need matched account; Pro rate not guaranteed EEA instant-convert rate.
- [IBKR US stocks fees](https://www.interactivebrokers.com/en/pricing/commissions-stocks.php), [country list](https://www.interactivebrokers.com/en/accounts/open-account-country-list.php), [PRIIPs lesson](https://www.interactivebrokers.com/campus/trading-lessons/us-taxes-for-us-non-residents-3/): published commissions/access/KID restrictions. Fractional order/account-specific pricing not assumed universal.
- [Alpaca countries](https://alpaca.markets/support/countries-alpaca-is-available): support/eligibility verification required; no complete guaranteed list inferred.
- [Webull EU OpenAPI SDK](https://developer.webull.eu/apis/docs/sdk/), [official Python SDK](https://github.com/webull-inc/webull-openapi-python-sdk): API exists; specific account/country/market permission not verified.

## 8. Wallet, network, data and model pricing

- [Jupiter official Swap v2 order/execute source](https://developers.jup.ag/docs/swap/order-and-execute): current route/API fields and fee buckets; concrete payload must be independently verified.
- [Solana fee structure](https://solana.com/docs/core/fees/fee-structure): lamport/base/priority model; USD estimate is scenario until SOL price/compute quote observed.
- [Solana Go SDK](https://github.com/solana-foundation/solana-go): maintained SDK candidate; exact version/module/Token-2022 handling not yet tested.
- [AWS KMS API Sign](https://docs.aws.amazon.com/kms/latest/APIReference/API_Sign.html), [Edwards support announcement](https://aws.amazon.com/about-aws/whats-new/2025/11/aws-kms-edwards-curve-digital-signature-algorithm/), [KMS cryptography](https://docs.aws.amazon.com/kms/latest/developerguide/kms-cryptography.html): Ed25519 supported, Pure/prehash distinction and key management; not a transaction firewall or proof chosen EU region works.
- [Safe modules](https://docs.safe.global/advanced/smart-account-modules), [Squads spending limits](https://docs.squads.so/main/navigating-your-squad/settings/spending-limits): actual authorization boundaries; transfer allowances do not automatically restrict arbitrary swaps.
- [SEC company-data APIs](https://www.sec.gov/search-filings/edgar-application-programming-interfaces), [10req/sec fair-access rule](https://www.sec.gov/filergroup/announcements-old/new-rate-control-limits): automated access/timestamp primitives, not intraday PIT guarantee of any downstream adapter.
- [Claude official pricing](https://platform.claude.com/docs/en/about-claude/pricing): Haiku4.5 $1/$5, Sonnet5 and5.5 $2/$10 per1M input/output, regional premium conditions. Scenario token counts in plan are our assumptions, not provider benchmark; actual billed tokens/retries unknown.

## 9. Evidence deliberately not claimed

1. No authenticated xChange/Ondo quote, reserved RFQ, real DEX fill or fee measurement was made.
2. No KYC, legal client approval, tax-country determination or production region contract was completed.
3. No upstream test suite, paid LLM benchmark, Nautilus adapter test or wallet signature was executed.
4. No independent backtest, prospective paper or live alpha was generated by this research.
5. Cost scenarios, token allocations, VPS/data budgets, loss caps and engineering effort are labelled design estimates, not current vendor quotations.
6. Core source review is not an exhaustive security audit of repositories/contracts. Pinned source, open issues and test fixtures are evidence for planned checks.

Readiness: исследовательский план готов; implementation и empirical success остаются отдельными незавершёнными стадиями.


