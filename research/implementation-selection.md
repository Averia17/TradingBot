# Выбор репозиториев для реализации — 04.10.2026

Выбран набор совместимых компонентов, а не один готовый «идеальный бот». Результат выбора — два локальных checkout и работающий research adapter; прибыльность системы ещё не измерена.

| Role | Repository / current evidence | Decision |
|---|---|---|
| AI core | [TauricResearch/TradingAgents](https://github.com/TauricResearch/TradingAgents), stable [v0.6.0](https://github.com/TauricResearch/TradingAgents/releases/tag/v0.6.0), Apache-2.0 | Cloned `vendor/trading-agents`, branch `bot/integration`, installed editable. Own portfolio boundary outside upstream |
| Portfolio/evaluation | [nautechsystems/nautilus_trader](https://github.com/nautechsystems/nautilus_trader), stable [v1.231.0](https://github.com/nautechsystems/nautilus_trader/releases/tag/v1.231.0), LGPL-3.0-or-later | Cloned source reference; installed Windows Python3.12 wheel, actual cash/fill/position probe passed |
| DeFi routing | [jup-ag/docs](https://github.com/jup-ag/docs), [official Swap v2 order/execute](https://developers.jup.ag/docs/swap/order-and-execute) | Use generated/thin official HTTP adapter later; not another DEX router/service |
| Chain primitives | [solana-foundation/solana-go](https://github.com/solana-foundation/solana-go), [v2.1.0 go.mod](https://github.com/solana-foundation/solana-go/blob/v2.1.0/go.mod), Apache-2.0 | Future Go dependency **`github.com/solana-foundation/solana-go/v2`**; Go>=1.25. Do not use obsolete module path by assumption |
| Jupiter Go wrapper | [ilkamo/jupiter-go](https://github.com/ilkamo/jupiter-go), [generated client](https://github.com/ilkamo/jupiter-go/blob/main/jupiter/client.gen.go) | Not selected as runtime: inspected QuoteGet/SwapPost and Swap v1 references, no verified v2 order/execute compatibility |
| Memory ideas | [pipiku915/FinMem-LLM-StockTrading](https://github.com/pipiku915/FinMem-LLM-StockTrading), [xt2201/finmem](https://github.com/xt2201/finmem) | No second memory runtime installed; native TA baseline first. xt2201 API snapshot:0 stars, not declared GitHub fork; do not substitute its claims for canonical paper/reproducibility |
| Research alternatives | [FinAgent](https://github.com/DVampire/FinAgent), [AI Hedge Fund](https://github.com/virattt/ai-hedge-fund), [Qlib](https://github.com/microsoft/qlib), [VectorBT](https://github.com/polakowo/vectorbt) | Existing source review/reference; not added to MVP dependencies |

API activity snapshot: [implementation-repos-2026-10-04.json](implementation-repos-2026-10-04.json). TA109758 stars, Nautilus29622, solana-go1591, Jupiter-go126 on recheck; changing popularity is not alpha evidence. Full earlier comparison/source review in [plan](../tasks/plan.md).

## Important version details

- TA release tag points at `ff0d0b1b4d7240c16f07f363856639d526934e44`; previous research inspected merge commit `1394a3f…`. **Both have identical Git tree `8bd10e10ee9ba1dca4441596e123d3809e433480`**. Implementation pins tag/source commit rather than misleadingly treating moving main as release identity.
- One local TA patch fixes test cache isolation on Windows; source graph/agents untouched. Local commit `1e5dc5c23ac2e4a04377b49921cfcf85b6b25980`; patch reproducible from [windows-test-isolation.patch](../patches/trading-agents/windows-test-isolation.patch). GitHub remote fork/PR not published.
- Nautilus v1.231.0 source commit `27a8e54e7ac3c57d6cbf8891f0283dfbaee97317`. `develop` README describes v2 transition; stable wheel/API compatibility has been tested at selected release, not assumed from latest branch docs.
- Annotated tags must be dereferenced as `ref^{commit}`. Bootstrap checks commit, not tag-object SHA, and does not reset existing work.
- Jupiter source file is `swap/order-and-execute.mdx`; an earlier research link with `docs/swap/` returned404. Links corrected to current official developer page. API is `/swap/v2/order` and `/swap/v2/execute`; provider requirements and transaction validation still future work.

## What was actually run

Installed and locked dependencies; upstream isolated no-network suite; our contracts/adapter/CLI/callback/native-engine tests; synthetic CLI demo; source-SHA/patch bootstrap; wheel/sdist build. No wallet/seed, authenticated issuer quote, real paid LLM run, live order or alpha backtest. The first real-model test requires locally configured provider credential; this is stated in CLI help/README, not hidden behind synthetic output.

## Why this composition

Keep existing analyst/debate/provider/memory functionality; own only strict intent envelope, one global portfolio allocation, validation, research artifacts and budget control. Delegate existing orders/fills/positions mechanics to Nautilus. Keep future Go transaction verification/signing separate from AI. FinMem/VectorBT/extra runtimes need measured incremental benefit before expanding dependencies. Regulatory access, PIT integrity, execution economics and alpha gates remain in force.
