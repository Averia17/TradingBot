# Self-custody AI trader

TradingAgents анализирует рынок и предлагает решения, один portfolio manager определяет BUY/SELL/HOLD/ADD/REDUCE и размер. Наш тонкий адаптер выпускает строгий JSON и проверяет cash/exposure/evidence. Nautilus — готовый движок будущего portfolio backtest/paper. Go исполнение и isolated signer появятся после проверки сигналов, исполнения и доступа к venue.

**Сейчас реализована первая исследовательская веха, не live-бот.** Есть настоящий entry point для TradingAgents, офлайн fixture demo, JSON contract, portfolio checks, call budget и проверка native Nautilus engine. Реальные LLM решения требуют настроенного API; fake demo не выдаётся за анализ/alpha. Кошелёк не подключён.

## Выбранные компоненты

| Компонент | Выбор | Использование |
|---|---|---|
| AI core | TradingAgents v0.6.0 | Clean checkout `vendor/trading-agents`; наш адаптер снаружи |
| Evaluation engine | NautilusTrader v1.231.0 | `vendor/nautilus-trader` reference; pinned binary wheel runtime |
| Future swap routing | Official Jupiter Swap v2 | API adapter; без собственного DEX router |
| Future Solana primitives | Solana Foundation solana-go v2 | SDK, не собственная криптография |
| Memory | Native TradingAgents baseline | FinMem concepts только после ablation; не второй runtime |

Версии/SHA: [upstream.lock.json](upstream.lock.json), выбор и caveats: [repository selection](research/implementation-selection.md). Исходники upstream не меняем; совместимость нового release проверяется отдельно. Схема и процедура: [обновление upstream](docs/upstream-updates.md).

## Установка и запуск

Нужны Git и uv; Python 3.12 выбран в `.python-version`. Bootstrap клонирует два чистых репозитория, проверяет remote/HEAD/source SHA и ставит pinned зависимости. Локальные изменения в upstream отклоняются без сброса.

```powershell
.\scripts\bootstrap.ps1
uv run --frozen --extra evaluation tradingbot doctor
uv run --frozen --extra evaluation tradingbot demo
uv run --frozen --extra evaluation tradingbot probe-engine
uv run --frozen --extra evaluation pytest
```

Последний стабильный TradingAgents подключается одной командой. Она проверяет зависимости, адаптер, наши тесты и build, после чего фиксирует новый SHA; при ошибке возвращает прежнюю версию.

```powershell
.\scripts\update-upstream.ps1 -CheckOnly
.\scripts\update-upstream.ps1
```

Ежедневный CI проверки latest stable подготовлен в `.github/workflows/`; активируется после размещения проекта на GitHub. Наши расширения пишем в `src/tradingbot/`, upstream файлы остаются чистыми.

`demo`: TSLA/NVDA, $1 000 USDC, синтетический HOLD с unknown forecast/confidence, 0 API calls. Artifacts в уникальном `runtime/demo-*`. `probe-engine`: реальные Nautilus cash/position/fill operations на искусственных quotes и заранее заданных orders; не подсоединён к AI intents и не backtest доходности.

## Настоящий research анализ

Настроить API credential выбранного существующего TradingAgents provider локально в `.env` или окружении. `.env.example` содержит только пустые placeholders; seed/private keys не нужны. Обновить `snapshot_id` и `as_of` в копии [portfolio-1000.json](examples/portfolio-1000.json) на действительный snapshot. Цена USDC в примере условно $1; depeg accounting и executable quotes ещё не реализованы.

```powershell
uv run --frozen --extra evaluation tradingbot analyze --portfolio examples/portfolio-1000.json --symbols TSLA NVDA --max-calls 64 --output runtime/research-001
```

При необходимости `--config my-models.json` задаёт только `llm_provider`, `quick_think_llm`, `deep_think_llm` и optional tier provider/endpoints, поддержанные upstream. Не размещать ключи в этом JSON. Текущие default models берутся из pinned upstream, доступ и EU processing нужно подтвердить отдельно.

Путь: три upstream analysts → bull/bear → trader/risk/PM для каждой бумаги → **один typed portfolio arbitration** → проверенный research batch. Это дополнительный allocator вокруг существующего graph, не новый agent framework. Outputs: `intents.json`, `portfolio.json`, `reports.json`, `metadata.json`. Output directories не перезаписываются. Decimal amounts сериализуются строками для точной передачи в будущий Go слой.

`max-calls` — shared thread-safe ограничение provider calls, не гарантированная сумма USD. SDK retries выключены; tool rounds/output tokens ограничены. Usage сохраняется где provider возвращает metadata; USD cost пока null. Structured output failure/unknown evidence/risk mismatch → error, без BUY по умолчанию и без free-text fallback в наш contract.

## Границы первой вехи

- `execution_mode` в schema **только `research`**; signer/submit/live commands отсутствуют.
- Cash/concentration/holding/action checks не заменяют production Go risk kernel и reservations ledger.
- Upstream data имеет day-level cutoff, не verified intraday PIT. Исторические LLM результаты могут содержать model knowledge leakage.
- Forecast/confidence — оценки модели, не calibration или доказанная alpha. Котировок real execution пока нет.
- Native memory/reflection upstream имеет своё fixed-window settlement; это baseline, не наш фактический net position P&L.
- Real paid LLM run ещё не проверен. Offline integration и native-engine probe не заменяют его.
- Postgres ledger, chronological PIT replay, cost-aware fills, asset registry, Go execution/security и forward paper — следующие части [плана](tasks/plan.md) и [backlog](tasks/todo.md).

## Проверки

```powershell
uv run --frozen --extra evaluation pytest
uv run --frozen --extra evaluation ruff check src tests
uv run --frozen --extra evaluation ruff format --check src tests
uv build --no-sources
```

Upstream suite отдельно из `vendor/trading-agents`: `..\..\.venv\Scripts\python.exe -m pytest tests -q -m "not integration"`. Network в этой suite блокируется fixtures; platform/optional-provider skips учитываются отдельно. [Отчёт первой вехи](docs/implementation-m1.md).
