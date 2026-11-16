# Self-custody AI trader

TradingAgents анализирует рынок и предлагает решения, один portfolio manager определяет BUY/SELL/HOLD/ADD/REDUCE и размер. Наш тонкий адаптер выпускает строгий JSON и проверяет cash/exposure/evidence. Nautilus — готовый движок будущего portfolio backtest/paper. Go исполнение и isolated signer появятся после проверки сигналов, исполнения и доступа к venue.

**Сейчас реализована первая исследовательская веха, не live-бот.** Есть настоящий entry point для TradingAgents, офлайн fixture demo, JSON contract, portfolio checks, call budget и проверка native Nautilus engine. Реальные LLM решения требуют настроенного API; fake demo не выдаётся за анализ/alpha. Кошелёк не подключён.

В архитектуре **Go services + Python adapters** Go отвечает за control/state/risk/execution, Python — за TradingAgents и Nautilus. Локальный Go control проверяет research JSON и с настроенной PostgreSQL принимает, читает и выдаёт research jobs worker по lease. [Архитектура](docs/architecture.md), [переход](docs/planning/service-migration.md), [полный план](docs/planning/plan.md).

| Каталог сервиса | Назначение |
|---|---|
| [`services/research`](services/research/) | Python CLI, адаптеры TradingAgents и Nautilus, тесты и upstream locks |
| [`services/control`](services/control/) | Go API, research jobs, PostgreSQL и тесты |
| [`services/trade-events`](services/trade-events/) | Тестовый приём исполнений, pgqueue outbox и публикация в Kafka |
| [`services/notifications`](services/notifications/) | Kafka consumer и личные уведомления в Telegram |

Код, зависимости и тесты принадлежат своим сервисам. В [`contracts/`](contracts/)
лежат JSON-схемы и примеры payload. Конфигурация Compose — в [`deploy/`](deploy/).

## Выбранные компоненты

| Компонент | Выбор | Использование |
|---|---|---|
| AI core | TradingAgents v0.7.1 | Clean checkout `vendor/trading-agents`; наш адаптер снаружи |
| Evaluation engine | NautilusTrader v1.231.0 | `vendor/nautilus-trader` reference; pinned binary wheel runtime |
| Future swap routing | Official Jupiter Swap v2 | API adapter; без собственного DEX router |
| Future Solana primitives | Solana Foundation solana-go v2 | SDK, не собственная криптография |
| Memory | Native TradingAgents baseline | FinMem concepts только после ablation; не второй runtime |

Версии и SHA в [upstream.lock.json](services/research/upstream.lock.json), обоснование в [repository selection](docs/research/implementation-selection.md). Процедура проверки нового релиза описана в [обновлении upstream](docs/upstream-updates.md).

## Установка и запуск

Нужны Git и uv; Python 3.12 выбран в `.python-version`. Bootstrap клонирует два чистых репозитория, проверяет remote/HEAD/source SHA и ставит pinned зависимости. Локальные изменения в upstream отклоняются без сброса.

```powershell
.\services\research\scripts\bootstrap.ps1
uv --directory services/research run --frozen --extra evaluation tradingbot doctor
uv --directory services/research run --frozen --extra evaluation tradingbot demo
uv --directory services/research run --frozen --extra evaluation tradingbot probe-engine
```

Последний стабильный TradingAgents подключается одной командой. Она проверяет зависимости, адаптер, наши тесты и build, после чего фиксирует новый SHA; при ошибке возвращает прежнюю версию.

```powershell
.\services\research\scripts\update-upstream.ps1 -CheckOnly
.\services\research\scripts\update-upstream.ps1
```

GitHub Actions ежедневно проверяет latest stable TradingAgents. `Quality` проверяет Python на Linux и Windows и три Go-модуля на Linux. [Описание CI](.github/README.md).

`demo`: TSLA/NVDA, $1 000 USDC, синтетический HOLD с unknown forecast/confidence, 0 API calls. Artifacts в уникальном `runtime/demo-*`. `probe-engine`: реальные Nautilus cash/position/fill operations на искусственных quotes и заранее заданных orders; не подсоединён к AI intents и не backtest доходности.

## Локальный Go control

```powershell
go -C services/control run ./cmd/control
```

По умолчанию адрес `127.0.0.1:8080`. `GET /healthz` проверяет liveness,
`GET /v1/capabilities` показывает поддерживаемые возможности.
`POST /v1/research/validate` принимает JSON с полями `batch`, `portfolio`
и `requested_symbols`. После проверки возвращает `{"valid":true}`.
Максимальный размер запроса 256 KiB, базовый предел возраста research — 1 час.
Это локальная диагностика на данных вызывающей стороны.

Чтобы включить локальное хранилище заданий, перед запуском задайте
`TRADINGBOT_DATABASE_URL` для выделенной PostgreSQL. Control проверяет доступ и
применяет миграции при старте. `GET /readyz` проверяет доступность
настроенной базы. Локальный API работает с доверенного окружения, настройка
межсервисного доступа входит в этап deployment.

`POST /v1/research/jobs` принимает JSON с `job_schema_version: "0.1"`,
`job_id` (UUID), `portfolio`, `requested_symbols`, `data_manifest`
(`ref` и `sha256`), `policy_version` и `max_llm_calls` (1–64).
После commit возвращает `201` и `Location`. Задание читается через
`GET /v1/research/jobs/{id}`. Повтор с тем же каноническим payload возвращает
`200` с сохранённым заданием, даже если исходный snapshot уже устарел. Повтор
ID с другим содержимым возвращает `409`. UUID приводятся к нижнему регистру,
`as_of` — к UTC. Порядок массивов и запись decimal strings сохраняются.
Control проверяет формат `data_manifest` и сохраняет ссылку и хеш. Чтение
файла и проверка его содержимого входят в будущую интеграцию с архивом.

Worker вызывает `POST /v1/research/jobs/claim` с `worker_id` и UUID `claim_id`.
Ответ `200` содержит исходное задание, номер попытки, `lease_token`,
`lease_expires_at` и `attempt_deadline`. При неопределённом ответе повторите
запрос с тем же worker и claim ID. При `204` заданий к выдаче сейчас нет,
и тот же claim ID можно использовать для следующей проверки.

Для продления worker вызывает `POST /v1/research/jobs/{id}/heartbeat`
с `worker_id` и `lease_token`. Повтор claim и heartbeat для истёкшего или
чужого lease возвращают `409`. Lease выдаётся на срок до 60 секунд, общий
предел попытки — 15 минут или остаток срока свежести снимка. После истечения
lease задание может получить новую попытку с новым токеном, всего до трёх.
Состояния `expired` и `failed` фиксируются при обходе очереди.
[JSON Schema](contracts/research-job.schema.json) и
[примеры запросов](contracts/valid-lease-requests.json).

`POST /v1/research/jobs/{id}/complete` принимает `result_schema_version: "0.1"`,
`worker_id`, `lease_token`, `batch`, `reports` и `metadata`. Версия `0.1`
принимает `synthetic_fixture`. Версия `0.2` принимает результат графа TradingAgents
с тестовыми моделями, наблюдаемыми вызовами и происхождением результата.
Control проверяет batch по сохранённым portfolio и universe, а хеши evidence
по тексту отчётов. Запись результата, статус `completed` и событие outbox
фиксируются одной транзакцией. Первый запрос возвращает `201`. Повтор той же
попытки с тем же содержимым возвращает прежний результат с `200`, в том числе
после истечения lease. `GET /v1/research/jobs/{id}/result` читает результат.
[JSON Schema результата](contracts/research-result.schema.json).

### Офлайн worker для research jobs

Для отдельного локального Go control с тестовыми заданиями и настроенной
PostgreSQL запустите в другом терминале:

```powershell
go -C services/control run ./cmd/research-fixture-worker --control-url http://127.0.0.1:8080
```

Worker опрашивает тестовую очередь, продлевает lease, строит синтетический
`HOLD` для каждого символа из сохранённого снимка и возвращает `synthetic_fixture`
с нулём LLM-вызовов. Он обращается к Go control по HTTP. Настройки базы нужны только
control. Сохранённый результат доступен через `GET /v1/research/jobs/{id}/result`.
При потере ответа worker повторяет claim с тем же ID и отправляет прежний payload
completion. Если lease утрачен, новая публикация отменяется. `Ctrl+C`
останавливает worker.

Для проверки ошибок транспорта и идемпотентности используйте отдельную тестовую
PostgreSQL и `TRADINGBOT_TEST_DATABASE_URL`, затем выполните Go-тесты control.

### Офлайн worker с графом TradingAgents

Для локальной тестовой очереди запустите worker из окружения с чистым pinned
checkout TradingAgents и установленными зависимостями research:

```powershell
uv --directory services/research run --frozen tradingbot worker --control-url http://127.0.0.1:8080
```

Worker использует модель `scripted-offline-v1`. В результате видны число
вызовов внутри графа, наблюдаемые токены, число вызовов без usage, версия и SHA
upstream, хеш конфигурации, манифест и номер попытки. Внешние модели и источники
данных не вызываются. На тестовой очереди запускайте один из двух воркеров
в зависимости от требуемого вида результата.

До каждого вызова Python отправляет `POST /v1/research/jobs/{id}/authorize-call`
с `worker_id`, `lease_token` и UUID `call_id`. Control проверяет действующую
аренду, записывает разрешение в PostgreSQL и возвращает `201`. Если ответ
потерян, повтор с тем же ID и действующей арендой возвращает `200` и прежнюю
квитанцию. В ней есть `remaining_llm_calls` на момент выдачи разрешения.
`422 budget_exhausted` останавливает модель до вызова. Израсходованный слот
остаётся списанным даже при сбое между записью разрешения и вызовом модели.
Успешный результат `0.2` должен содержать число вызовов, равное числу
разрешений текущей попытки. Общий лимит действует на все попытки задания.
USD billing и подтверждение полноты и прав на рыночные данные входят в следующие срезы.

Для сквозного прогона укажите `TRADINGBOT_E2E_DATABASE_URL` отдельной пустой
PostgreSQL и запустите `pytest tests/test_worker_postgres.py` из окружения
research. Тест собирает Go control и запускает worker отдельным процессом.

В режиме `tradingbot worker --archive-root <путь>` worker сверяет SHA-256
`data_manifest` с локальным файлом, выбирает только доступные к `as_of`
ревизии и передаёт ограниченный источниками контекст в TradingAgents. Хеш
выбранного снимка входит в отчёт. Пример формата есть в
[синтетическом архиве](services/research/examples/pit-archive.json).

## Локальный офлайн-контур в Docker Compose

Go control, PostgreSQL и Python worker запускаются из корня репозитория.
Control доступен на `127.0.0.1:18080`. PostgreSQL доступна только control по
отдельной внутренней сети. Research работает в сети без внешнего доступа, а
профиль `evaluation` запускает Nautilus без сетевого интерфейса Docker.

```powershell
docker compose -f deploy/research/compose.yaml up -d --build --wait
docker compose -f deploy/research/compose.yaml --profile evaluation run --rm --build evaluation
```

Для отдельной проверки сборки, совместимости, замены и отката worker запустите
`python deploy/research/smoke.py` с Python 3.12 и Docker. Скрипт создаёт временный
Compose-проект, отправляет тестовые задания, проверяет отклонение неподдерживаемой
версии результата и удаляет только свои контейнеры и БД. Он выводит ID образов,
версии протоколов, SHA TradingAgents и хеши lock-файлов. Для постоянного локального
проекта используйте `docker compose -f deploy/research/compose.yaml down` без
удаления тома PostgreSQL.

При обновлении сначала остановите старый `research`. Активная попытка с
неподтверждённым результатом восстанавливается через истечение lease, а ранее
выданные разрешения на вызовы учитываются. Установите `TRADINGBOT_RESEARCH_IMAGE`
на проверенный тег или digest и выполните `up -d --no-deps --no-build --force-recreate research`
для того же Compose-файла. Для отката используйте идентификатор прежнего образа.
Control, оценка и том БД при этом остаются на месте.

### Виртуальный paper-счёт

Control с PostgreSQL может создать виртуальный счёт через `POST /v1/paper/accounts`
с `account_id` и `initial_cash_usd` в виде строки. Чтение счёта доступно через
`GET /v1/paper/accounts/{id}`. Резервируйте полную BUY или SELL заявку через
`POST /v1/paper/accounts/{id}/orders`. Она связана с версией счёта, инструментом,
ID решения и котировки, предельной комиссией и сроком. `POST .../orders/{order}/resolve`
пишет только виртуальный исход `filled`, `unknown` или `cancelled`. Неопределённый
исход сохраняет резерв до сверки. Для журнала используйте
`GET /v1/paper/accounts/{id}/events?after_version=0&limit=25`.
`GET /v1/paper/accounts/{id}/outcomes?as_of=<UTC>` восстанавливает FIFO-результаты
с учётом fees и себестоимости ещё открытых лотов по доступной на тот момент
версии журнала. Для `as_of` передавайте RFC3339, например `2026-11-16T17:00:00Z`.
[JSON Schema](contracts/paper-account.schema.json).

Этот API принимает тестовые данные для локального исполнения. Проверки денег и
количеств выполняет PostgreSQL. Команда `go -C services/control run ./cmd/paper-replay
--intent <intent.json> --quote <quote.json>` применяет к ledger локальную тестовую
котировку, которую intent привязывает по SHA-256. Нужна `TRADINGBOT_DATABASE_URL`
для Control DB с созданным paper account. Повторы используют устойчивые ID.
Политика концентрации и частичные исполнения дорабатываются в S5.
[Формат входа и результата](contracts/paper-replay.schema.json).

Для создания заданий по готовому расписанию запустите `go -C services/control run
./cmd/research-scheduler --once --config <schedule.json> --archive-root <путь>`.
Планировщик проверяет хеш архива и вычисляет ID из расписания и времени события.
Повтор того же запуска возвращает существующее задание. Для собственного прогона
укажите актуальные `scheduled_at` и `portfolio.as_of` и вычислите SHA-256 текущих
байтов архива. Формат показан в [примере расписания](contracts/valid-research-schedule.json).

Независимый Nautilus replay читает подготовленные тестовые котировки и заявки.
Денежные дивиденды учитываются по времени права и выплаты. `benchmarks` на том же
горизонте сравнивает cash, buy-and-hold и равную начальную аллокацию.

```powershell
uv --directory services/evaluation run --frozen tradingbot-evaluation replay examples/quote-replay.json
uv --directory services/evaluation run --frozen tradingbot-evaluation benchmarks examples/quote-replay.json --start-at 2026-10-02T13:30:10Z --buy-and-hold-symbol TSLA
```

## Уведомления о сделках

В [отдельном Compose-проекте](deploy/notifications/compose.yaml) сервис
[`trade-events`](services/trade-events/) принимает тестовый отчёт биржи и
публикует событие через pgqueue outbox в Kafka. Сервис
[`notifications`](services/notifications/) отправляет его в личный Telegram-чат.
У каждого сервиса отдельные Go-модуль, Dockerfile и образ. Формат сообщения
задаёт [JSON Schema](contracts/trade-event.schema.json).
Compose запускает собственные PostgreSQL и Kafka. Тестовый профиль открывает
HTTP-ручку на `127.0.0.1:18082`. Тестовые сообщения идут в отдельный topic
`tradingbot.trade.filled.fixture.v1` и помечаются в Telegram.

Compose передаёт `TRADINGBOT_TELEGRAM_BOT_TOKEN` и числовой
`TRADINGBOT_TELEGRAM_CHAT_ID` в контейнер `notifications` через secrets.
Для первоначальной настройки нажмите **Start** в личном чате с ботом и
возьмите `message.chat.id` из ответа Telegram
`getUpdates`. Для уже настроенного Windows-аккаунта загрузите эти значения из DPAPI.
[Переменные Compose-проекта](deploy/notifications/.env.example).

Запуск и отправка одного тестового исполнения из корня репозитория.

```powershell
& "$env:LOCALAPPDATA\TradingBot\Load-TelegramEnvironment.ps1"
docker compose -f deploy/notifications/compose.yaml --profile test up -d --build --wait

$fill = Get-Content contracts/valid-trade-event.json -Raw | ConvertFrom-Json
$fill.id = "paper:exchange-test:$([guid]::NewGuid().ToString('N'))"
$fill.filled_at = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ss.fffZ')
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18082/v1/test/exchange/fills `
    -ContentType application/json -Body ($fill | ConvertTo-Json -Compress)
docker compose -f deploy/notifications/compose.yaml --profile test logs -f trade-events notifications
```

Чтобы пересобрать или обновить один сервис, укажите его имя в `build` или
`up -d --build`. Для `trade-events` нужен профиль `test`.

HTTP `201` подтверждает commit тестового fill и outbox. Повтор того же payload
даёт `200` с прежней записью, изменение под тем же ID даёт `409`. Подключение
к подтверждённым сделкам входит в S5 вместе с ledger и reconciliation.
[Гарантии доставки и восстановление](docs/decisions/002-trade-notifications.md).

Если Docker Hub недоступен, `TRADINGBOT_GO_BUILD_IMAGE` позволяет задать зеркало
того же образа Go. Локальная сборка проверена с
`mirror.gcr.io/library/golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd`.

## Настоящий research анализ

Настроить API credential выбранного TradingAgents provider в `services/research/.env` или окружении. [Пример окружения](services/research/.env.example). Обновить `snapshot_id` и `as_of` в копии [portfolio-1000.json](services/research/examples/portfolio-1000.json) на действительный snapshot. Цена USDC в примере условно $1.

```powershell
uv --directory services/research run --frozen --extra evaluation tradingbot analyze --portfolio examples/portfolio-1000.json --symbols TSLA NVDA --max-calls 64 --output runtime/research-001
```

При необходимости `--config my-models.json` задаёт только `llm_provider`, `quick_think_llm`, `deep_think_llm` и optional tier provider/endpoints, поддержанные upstream. Не размещать ключи в этом JSON. Текущие default models берутся из pinned upstream, доступ и EU processing нужно подтвердить отдельно.

Путь: три upstream analysts → bull/bear → trader/risk/PM для каждой бумаги → **один typed portfolio arbitration** → проверенный research batch. Outputs: `intents.json`, `portfolio.json`, `reports.json`, `metadata.json`. Output directories не перезаписываются. Decimal amounts сериализуются строками для точной передачи в Go.

`max-calls` — shared thread-safe ограничение provider calls, не гарантированная сумма USD. SDK retries выключены; tool rounds/output tokens ограничены. Usage сохраняется где provider возвращает metadata; USD cost пока null. Structured output failure/unknown evidence/risk mismatch → error, без BUY по умолчанию и без free-text fallback в наш contract.

## Границы первой вехи

- `execution_mode` в schema **только `research`**; signer/submit/live commands отсутствуют.
- Cash/concentration/holding/action checks не заменяют production Go risk kernel и reservations ledger.
- Upstream data имеет day-level cutoff, не verified intraday PIT. Исторические LLM результаты могут содержать model knowledge leakage.
- Forecast/confidence — оценки модели, не calibration или доказанная alpha. Котировок real execution пока нет.
- Native memory/reflection upstream имеет своё fixed-window settlement; это baseline, не наш фактический net position P&L.
- Real paid LLM run ещё не проверен. Offline integration и native-engine probe не заменяют его.
- Postgres ledger, chronological PIT replay, cost-aware fills, asset registry, Go execution/security и forward paper — следующие части [плана](docs/planning/plan.md) и [backlog](docs/planning/todo.md).

## Проверки

```powershell
uv --directory services/research run --frozen --extra evaluation pytest
uv --directory services/research run --frozen --extra evaluation ruff check tradingbot tests scripts
uv --directory services/research build --no-sources
go -C services/control test -mod=readonly ./...
go -C services/trade-events test -mod=readonly ./...
go -C services/notifications test -mod=readonly ./...
```

Чтобы включить PostgreSQL-тесты Go, задайте `TRADINGBOT_TEST_DATABASE_URL`
для отдельной тестовой базы. Тесты создают и удаляют в ней собственные схемы.

Upstream suite запускается отдельно из `services/research/vendor/trading-agents`
через `uv run --project ../.. --frozen --extra evaluation pytest -c pyproject.toml tests`.
[Результаты проверок](docs/verification.md).
