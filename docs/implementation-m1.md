# Milestone 1 — pinned core and research boundary

Исторический отчёт первоначального M1. Текущий процесс заменён на
[clean upstream + adapters](upstream-updates.md): patch больше не применяется,
а официальный release-tag SHA уточнён до `1394a3f` вместо одноимённой release
ветки `ff0d0b1`. Их Git tree идентичны (`8bd10e1`); исходные runtime файлы те же.

**Date:**04.10.2026. **Result:** первая research implementation готова к запуску на выбранном API provider, offline path проверен. Это не completed autonomous live trader.

## Delivered

1. Cloned two selected official repos; pinned release SHA/versions in `upstream.lock.json`; TA local branch `bot/integration`, upstream remote retained.
2. Python3.12 environment, installed TA0.6.0/Nautilus1.231.0, `uv.lock`, executable bootstrap and buildable package.
3. Decimal financial contracts for marked portfolio, reserved cash, five actions, horizon hours, model estimates, source references and trusted decision IDs.
4. Adapter calls actual `TradingAgentsGraph.propagate(..., portfolio=PortfolioContext(...))`; global structured allocator sees all requested reports and shared cash. No invented expected-return/confidence conversion from a rating.
5. Fail-closed validation: symbol coverage, duplicates, no short, BUY/ADD/SELL/REDUCE consistency, concentration, cash across all buys, no use of unfilled sales, missing/fabricated evidence and invalid typed output.
6. Thread-safe LLM call budget with actual LangChain callback-path check; retries disabled; observed usage tracked, dollar billing explicitly not implemented.
7. Research CLI: `demo`, `analyze`, `doctor`, `schema`, `probe-engine`. Exact schema exported to `shared/trade-intent/schema.json`.
8. Synthetic Nautilus probe uses native cash account, quote events, market orders, fills and positions: TSLA1 and NVDA2 at artificial $100 ask, $700 cash remains. It is not connected to AI intents and not an alpha backtest.

## Verification and root cause evidence

- Our full suite: **29 passed**,2 upstream Pandas warnings. Network disabled by autouse fixtures. Financial boundaries, typed failure, JSON output/overwrite protection, callback cap and native engine are covered. JUnit: `runtime/verification/our.xml`.
- TA stable upstream initial run:1258 passed,1 failed,4 skipped,1 deselected,99 subtests. Failed assertion required cache path outside `Path.home()`. On Windows normal temporary directory is under `C:/Users/User/AppData/Local/Temp`, so assertion tested filesystem layout rather than isolation.
- Fix: assert dedicated suite temp directory parent and `tradingagents-tests-yf-` prefix. No network guard removed, no test skipped or weakened into unconditional pass. Focused isolation suite:9 passed.
- Complete TA suite after patch: **1259 passed,4 skipped,1 deselected,99 subtests**. Three skips are POSIX file-mode tests, one absent optional Bedrock dependency; integration marked test excluded. Warnings from deliberate unknown-model fixtures retained.
- Native Nautilus probe passes; emits two upstream Pandas4Warning messages for deprecated `Timestamp.utcnow` under required pandas3.0.6. No suppression or dependency downgrade; future upgrade compatibility remains a tracked caveat.
- Bootstrap verifies source commit (including annotated-tag dereference) and idempotent patch; component doctor reports versions0.1.0/0.6.0/1.231.0.
- Wheel/sdist built with `uv build --no-sources`; synthetic demo archive exists in `runtime/m1-demo`; engine result in `runtime/m1-engine-probe.json`. Runtime directories ignored by Git. `ruff check`, `ruff format --check` pass; `uv pip check` confirms102 installed packages compatible.

## Review boundaries

Reviewed correctness, simplicity, dependency compatibility, external-input handling and resource limits using code-review skill. This is a self-review of the first small research component, not an independent production security audit. No privileged wallet/API execution capability exists; schema permits only research. AI estimates aren't calibrated, reports aren't verified intraday PIT, actual quote costs unavailable, SQL reservations/storage not implemented.

Common model API keys are **not configured** in the inspected environment. No paid analysis was attempted; no expected alpha/real recommendations were manufactured. `analyze` entry point is wired to real upstream/provider APIs but awaits credentials and an actual model compatibility test. Store credential locally; never provide wallet seed.

## Next vertical slice

Run bounded actual TSLA/NVDA analysis with a locally configured provider; verify strict portfolio output, actual call/token counts and fail-closed schema compatibility. Then connect persisted intents and archived PIT inputs to chronological Nautilus portfolio replay with costs/corporate actions. Do not write DEX routing or wallet signing before these are verified. Native TA fixed-window reflection remains a research baseline, not realized net trade memory.
