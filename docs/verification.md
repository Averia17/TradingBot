# История проверок TradingBot

Пути и результаты в этом документе относятся к состоянию репозитория на дату прогона. Текущая структура описана в [архитектуре](architecture.md).

## Milestone 1

Исторический отчёт первоначального M1. Текущий процесс заменён на
[clean upstream + adapters](upstream-updates.md): patch больше не применяется,
а официальный release-tag SHA уточнён до `1394a3f` вместо одноимённой release
ветки `ff0d0b1`. Их Git tree идентичны (`8bd10e1`); исходные runtime файлы те же.

**Date:**04.10.2026. **Result:** первая research implementation готова к запуску на выбранном API provider, offline path проверен. Это не completed autonomous live trader.

### Delivered

1. Cloned two selected official repos; pinned release SHA/versions in `upstream.lock.json`; TA local branch `bot/integration`, upstream remote retained.
2. Python3.12 environment, installed TA0.6.0/Nautilus1.231.0, `uv.lock`, executable bootstrap and buildable package.
3. Decimal financial contracts for marked portfolio, reserved cash, five actions, horizon hours, model estimates, source references and trusted decision IDs.
4. Adapter calls actual `TradingAgentsGraph.propagate(..., portfolio=PortfolioContext(...))`; global structured allocator sees all requested reports and shared cash. No invented expected-return/confidence conversion from a rating.
5. Fail-closed validation: symbol coverage, duplicates, no short, BUY/ADD/SELL/REDUCE consistency, concentration, cash across all buys, no use of unfilled sales, missing/fabricated evidence and invalid typed output.
6. Thread-safe LLM call budget with actual LangChain callback-path check; retries disabled; observed usage tracked, dollar billing explicitly not implemented.
7. Research CLI: `demo`, `analyze`, `doctor`, `schema`, `probe-engine`. Exact schema exported to `shared/trade-intent/schema.json`.
8. Synthetic Nautilus probe uses native cash account, quote events, market orders, fills and positions: TSLA1 and NVDA2 at artificial $100 ask, $700 cash remains. It is not connected to AI intents and not an alpha backtest.

### Verification and root cause evidence

- Our full suite: **29 passed**,2 upstream Pandas warnings. Network disabled by autouse fixtures. Financial boundaries, typed failure, JSON output/overwrite protection, callback cap and native engine are covered. JUnit: `runtime/verification/our.xml`.
- TA stable upstream initial run:1258 passed,1 failed,4 skipped,1 deselected,99 subtests. Failed assertion required cache path outside `Path.home()`. On Windows normal temporary directory is under `C:/Users/User/AppData/Local/Temp`, so assertion tested filesystem layout rather than isolation.
- Fix: assert dedicated suite temp directory parent and `tradingagents-tests-yf-` prefix. No network guard removed, no test skipped or weakened into unconditional pass. Focused isolation suite:9 passed.
- Complete TA suite after patch: **1259 passed,4 skipped,1 deselected,99 subtests**. Three skips are POSIX file-mode tests, one absent optional Bedrock dependency; integration marked test excluded. Warnings from deliberate unknown-model fixtures retained.
- Native Nautilus probe passes; emits two upstream Pandas4Warning messages for deprecated `Timestamp.utcnow` under required pandas3.0.6. No suppression or dependency downgrade; future upgrade compatibility remains a tracked caveat.
- Bootstrap verifies source commit (including annotated-tag dereference) and idempotent patch; component doctor reports versions0.1.0/0.6.0/1.231.0.
- Wheel/sdist built with `uv build --no-sources`; synthetic demo archive exists in `runtime/m1-demo`; engine result in `runtime/m1-engine-probe.json`. Runtime directories ignored by Git. `ruff check`, `ruff format --check` pass; `uv pip check` confirms102 installed packages compatible.

### Review boundaries

Reviewed correctness, simplicity, dependency compatibility, external-input handling and resource limits using code-review skill. This is a self-review of the first small research component, not an independent production security audit. No privileged wallet/API execution capability exists; schema permits only research. AI estimates aren't calibrated, reports aren't verified intraday PIT, actual quote costs unavailable, SQL reservations/storage not implemented.

Common model API keys are **not configured** in the inspected environment. No paid analysis was attempted; no expected alpha/real recommendations were manufactured. `analyze` entry point is wired to real upstream/provider APIs but awaits credentials and an actual model compatibility test. Store credential locally; never provide wallet seed.

### Next vertical slice

Run bounded actual TSLA/NVDA analysis with a locally configured provider; verify strict portfolio output, actual call/token counts and fail-closed schema compatibility. Then connect persisted intents and archived PIT inputs to chronological Nautilus portfolio replay with costs/corporate actions. Do not write DEX routing or wallet signing before these are verified. Native TA fixed-window reflection remains a research baseline, not realized net trade memory.

## Clean upstream

### 2026-11-14

- В задаче проверки кандидата на Linux и Windows зафиксирован exit 2.
  Детальные логи недоступны анонимно. Та же команда updater локально
  воспроизвела Ruff `I001` в `src/tradingbot/contracts.py` после успешного
  `doctor`. [GitHub Actions run #41](https://github.com/Averia17/TradingBot/actions/runs/43590785716).
- После исправления импортов updater установил TradingAgents `v0.7.1`
  (`c4544913a14ed3df5f1ed7b9e8483270ce20682a`). Прошли dependency check,
  `doctor`, Ruff, 73 теста проекта и сборка wheel/sdist. Nautilus `v1.231.0`
  остаётся последним стабильным релизом. Оба checkout прошли `verify`.
- Исходный suite TradingAgents на Windows: 1407 passed, 6 skipped,
  1 deselected. Локальный JUnit — `runtime/verification/upstream-windows.xml`.
  Hosted запуск с этими изменениями ещё не проверен.
- После проверки Python/Go wire schema и нормализации денежных строк полный
  набор Python прошёл с результатом 83 passed. Остались два предупреждения
  Nautilus о deprecated `Timestamp.utcnow`.

### 2026-10-04

Уточнение 05.10.2026: local GitHub remote уже настроен. Выполнение hosted Actions
и server branch protection не проверены; прежнее указание об отсутствии remote
ниже исправлено. Counts и результаты runtime-проверок относятся к 04.10.2026.

Проверены изменения процедуры обновления и границы TradingAgents adapter.

- TradingAgents release `v0.6.0`, exact tag commit
  `1394a3f72aa4393e1a98f51b382434c4b4c2d972`; Nautilus `v1.231.0`,
  `27a8e54e7ac3c57d6cbf8891f0283dfbaee97317`.
- Оба checkout имеют правильный `upstream`, соответствуют manifest HEAD,
  не имеют tracked/untracked изменений; patches не применяются.
- `bootstrap.ps1`, `update-upstream.ps1 -CheckOnly` и настоящая команда
  `update-upstream.ps1` успешно выполнены. На момент запроса latest TA = v0.6.0.
- Updater выполнил разрешение/установку зависимостей, doctor, Ruff,
  полный suite и build. Отчёт:
  `runtime/upstream-updates/f213f05e-7376-4521-b6b4-f3c72e95ccf9.json`.
- После дополнительной проверки возобновления bootstrap: полный suite
  **69 passed**, Ruff без ошибок, `uv pip check` — 102 packages compatible,
  `uv lock --check` успешен, wheel/sdist собраны.
- 24 updater tests используют настоящие временные локальные Git репозитории:
  annotated tags, совпадающие имена ветки/тега, dirty/неверный remote/HEAD,
  retag, неизвестный SHA кандидата, static/dynamic version, rollback файлов и
  окружения, ошибка восстановления, ignored collision, concurrent lock,
  nonstable release refusal и безопасное возобновление пустого checkout.
- Adapter suite запускает настоящий upstream graph с scripted offline models;
  проверены свободный cash $850 и позиция TSLA 1.25 в фактических prompts.
  Все наши тесты запрещают socket/curl network; LLM кредиты не расходуются.
- Остались два существующих предупреждения deprecated `Timestamp.utcnow`
  из Nautilus во время native-engine probe; тест проходит, upstream не патчим.
- Workflows проверены YAML parsing и actionlint v1.7.12. Hosted GitHub runs и
  server branch protection здесь не проверялись; их статус не следует из YAML.
- Полный неизменённый upstream suite на Windows здесь не объявляется зелёным:
  его известная TEMP/HOME assumption описана в инструкции. Linux CI запускает
  стандартный изолированный suite дополнительно; это ещё предстоит серверной
  проверке. Real paid provider/live/alpha не проверены этим изменением.

Исходная branch/tag путаница уточнена по `git ls-remote`: release ветка
`refs/heads/v0.6.0` указывает на `ff0d0b1`, тег `refs/tags/v0.6.0` — на
`1394a3f`. Trees одинаковы `8bd10e10ee9ba1dca4441596e123d3809e433480`.
Текущее клонирование через init + exact-tag fetch устраняет неоднозначность.

Процедура: [upstream updates](upstream-updates.md).
