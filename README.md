# Self-custody AI trader

**Проект работает в режимах research и paper.** TradingAgents анализирует рынок, а portfolio manager выбирает BUY/SELL/HOLD/ADD/REDUCE и размер позиции. Go проверяет решения, выдаёт задания по lease и ведёт виртуальные счета в PostgreSQL. Nautilus воспроизводит исторические сценарии. Отчёты и уведомления о виртуальных исполнениях доступны в Telegram.

Вызовы реальной модели требуют настроенного API. Live execution и подписание остаются в плане. [Архитектура](docs/architecture.md), [этапы перехода](docs/planning/service-migration.md), [план](docs/planning/plan.md).

| Каталог сервиса | Назначение |
|---|---|
| [`services/research`](services/research/) | Python CLI, адаптеры TradingAgents и Nautilus, тесты и upstream locks |
| [`services/evaluation`](services/evaluation/) | Исторические прогоны Nautilus, эталоны и оценка расходов |
| [`services/control`](services/control/) | Go API, research jobs, PostgreSQL и тесты |
| [`services/trade-events`](services/trade-events/) | Тестовый приём исполнений, pgqueue outbox и публикация в Kafka |
| [`services/notifications`](services/notifications/) | Kafka consumer и личные уведомления в Telegram |
| [`services/portfolio-analytics`](services/portfolio-analytics/) | Go-проекция paper-счёта и отчёты NAV, P&L и доходности. [Запуск](docs/runbooks/paper.md#аналитика-портфеля-в-telegram) |
| [`services/venue-quotes`](services/venue-quotes/) | Go-сервис индикативных котировок. Стаканы Binance Spot и Dzengi, котировки Jupiter Swap v2. [Запуск](#котировки-площадки) |

Код, зависимости и тесты принадлежат своим сервисам. В [`contracts/`](contracts/)
лежат JSON-схемы и примеры payload. Конфигурация Compose — в [`deploy/`](deploy/).

## Выбранные компоненты

| Компонент | Выбор | Использование |
|---|---|---|
| AI core | TradingAgents v0.7.1 | Clean checkout `vendor/trading-agents`; наш адаптер снаружи |
| Evaluation engine | NautilusTrader v1.231.0 | `vendor/nautilus-trader` reference; pinned binary wheel runtime |
| Источник стакана | Dzengi, режим «Торги» | Публичный стакан токенизированных акций. [Сравнение площадок](docs/planning/plan.md#16-xstocks-vs-ondo-vs-broker) |
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

GitHub Actions ежедневно проверяет последнюю стабильную версию TradingAgents. `Quality` проверяет Python на Linux и Windows и пять Go-модулей на Linux. [Описание CI](docs/upstream-updates.md#автоматическая-проверка-свежести).

`demo`: TSLA/NVDA, $1 000 USDC, синтетический HOLD с unknown forecast/confidence, 0 API calls. Artifacts в уникальном `runtime/demo-*`. `probe-engine`: реальные Nautilus cash/position/fill operations на искусственных quotes и заранее заданных orders; не подсоединён к AI intents и не backtest доходности.

## Котировки площадки

`venue-quotes` слушает `127.0.0.1:18083`. `GET /v1/order-book/binance`
читает публичный стакан Binance Spot, включая пары bStocks, например
`NVDABUSDT`. `GET /v1/order-book` читает стакан Dzengi, например `NVDA`.
Оба маршрута работают без аккаунта и API-ключа. Запрос принимает биржевой
`symbol` и `depth` из `5`, `10`, `20`, `50`, `100`, `500`. Глубина по умолчанию
составляет пять уровней с каждой стороны. Адрес задаётся в `TRADINGBOT_QUOTE_LISTEN`.
При пустом `TRADINGBOT_QUOTE_PROVIDER` доступны все маршруты. Значения
`binance`, `dzengi` и `jupiter` включают только соответствующего провайдера.

```powershell
go -C services/venue-quotes run ./cmd/venue-quotes
```

Из другого терминала можно запросить стаканы NVDA на обеих площадках.

```powershell
Invoke-RestMethod -Uri 'http://127.0.0.1:18083/v1/order-book/binance?symbol=NVDABUSDT&depth=50' | ConvertTo-Json -Depth 5
Invoke-RestMethod -Uri 'http://127.0.0.1:18083/v1/order-book?symbol=NVDA&depth=5' | ConvertTo-Json -Depth 5
```

Ответ содержит `bids`, `asks`, цены и объёмы в decimal strings и локальное
время получения. Dzengi отдаёт `provider_timestamp` в миллисекундах, Binance
отдаёт `provider_sequence` из `lastUpdateId`. Стаканы имеют тип `indicative`.
Пустой стакан или неизвестная пара Binance возвращают
`404 order_book_unavailable`, ограничение запросов — `429`,
сбой провайдера — `502`, тайм-аут — `504`. Разрешения на торговлю, комиссии
конкретного аккаунта и исполнение проверяются отдельно.
[Контракт](contracts/venue-order-book.schema.json),
[пример Binance](contracts/valid-venue-order-book-binance.json),
[пример Dzengi](contracts/valid-venue-order-book.json),
[публичный API Binance](https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints),
[спецификация Dzengi](https://apimarketdoc.dzengi.com/v3/api-docs/default).

Для проверки заявок в режиме «Торги» нужен верифицированный аккаунт с торговым
API-доступом. Демо-аккаунт Dzengi поддерживает только режим с левереджем.
[Условия демо](https://help.dzengi.com/hc/ru/articles/7631879009169),
[подключение торгового API](https://dzengi.com/ru/api-get-started).

`POST /v1/quotes` остаётся доступен для исследования Solana через
[Jupiter Swap v2](https://developers.jup.ag/docs/swap). Запрос задаёт пару mint
и входную сумму в минимальных единицах токена.

В другом терминале из корня репозитория отправьте пример для входа в NVDAx на
100 USDC. Для обратной стороны поменяйте mint местами и задайте raw-сумму NVDAx.
При запросах без ключа соблюдайте паузу не менее двух секунд. Для API-ключа
провайдера используется переменная процесса `TRADINGBOT_JUPITER_API_KEY`.

```powershell
$request = Get-Content -Raw -LiteralPath contracts/valid-venue-quote-request.json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18083/v1/quotes -ContentType 'application/json' -Body $request
```

Ответ содержит расчётный выход и порог с учётом допуска на проскальзывание,
сообщённую комиссию свопа, маршрут, ID Jupiter и локальное время получения.
При отсутствии срока действия в ответе провайдера `provider_expires_at`
равен `null`.
Тип котировки — `indicative`. Сетевые затраты и стоимость исполнения
измеряются отдельно. Для paper fills требуется источник с подтверждённой
исполнимостью. [Контракт HTTP v1](contracts/venue-quote.schema.json) и
[синтетический пример нормализованного ответа](contracts/valid-venue-quote-observation.json).

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

Для общего потолка вызовов по всем заданиям задайте лимит на сутки UTC.
День и остаток можно прочитать через `research-budget show`. PostgreSQL
считает все выданные разрешения, включая разрешения по прерванным
попыткам и по другим заданиям. Повтор с исходным ID возвращает исходную
квитанцию. Лимит задаётся один раз на дату. С первой зарегистрированной
даты сутки без лимита блокируют новые вызовы.

```powershell
go -C services/control run ./cmd/research-budget show
go -C services/control run ./cmd/research-budget set --day $budgetUtcDay --max-calls 64
```

Перед первой активацией остановите экземпляры Control предыдущей версии.
Показатели относятся к количеству выданных разрешений. Выставленные
провайдером расходы учитываются отдельно.

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
и уже полученные локально ревизии. Он передаёт ограниченный контекст в
TradingAgents. Хеш выбранного снимка входит в отчёт. Пример формата есть в
[синтетическом архиве](services/research/examples/pit-archive.json).

Для импорта сохранённых JSON SEC EDGAR и ALFRED передайте манифест с SHA-256
исходных файлов. Импорт оставляет время приёма SEC и каждый accession,
включая поправки. Для ALFRED он сохраняет исходное значение и день винтажа.
Моментом доступности считается зафиксированное время получения ответа.
Запуск с локальными синтетическими примерами выводит архив в stdout.

```powershell
uv --directory services/research run --offline --frozen python -m tradingbot.ingest --root examples/pit-import --manifest examples/pit-import/manifest.json
```

Сохраните stdout в UTF-8 и вычислите SHA-256 итогового файла для
`data_manifest`. Импорт проверяет байты и переданные времена, а достоверность
сбора, права использования и покрытие требуют собственных подтверждений.
[Схема манифеста](contracts/pit-capture-manifest.schema.json) · [SEC EDGAR API](https://www.sec.gov/search-filings/edgar-application-programming-interfaces) · [Документация ALFRED](https://fred.stlouisfed.org/docs/api/fred/realtime_period.html).

Для количественных показателей SEC добавьте захват `sec_company_facts`.
Селектор задаёт accession, концепт US GAAP, единицу и отчётный период.
Показатель появляется в архиве после получения обоих исходных ответов.
Значение сохраняется точно в десятичной строке, без преобразования в float.
У поправки свой accession и своё время видимости.
[Синтетический пример манифеста](services/research/examples/pit-import/sec-facts-manifest.json).

Тот же импорт принимает `normalized_market` с исходными OHLCV, новостями
и их правками, корпоративными действиями и событиями листинга. Для каждого
файла фиксируются SHA-256 и местное время получения. Команда `snapshots`
с `--universe-provider` показывает состояние по устойчивому `security_id`.
Отбор версии 0.2 сверяет тикер со статусом листинга и принадлежностью
архивных записей выбранной бумаге. Пример использует синтетические данные.
[Контракт рыночного импорта](contracts/pit-market-capture.schema.json) · [Пример манифеста](services/research/examples/pit-import/market-manifest.json).

Для BLS добавлен импорт нормализованных наблюдений из двух публичных
выпусков. Он хранит дату официального релиза и время фактической сверки.
Архив относит запись к историческому срезу только после её сверки.
[Пример манифеста](services/research/examples/pit-import/bls-manifest.json) · [Правила и источники](docs/research/SOURCES.md).

Для предварительного отбора используйте SHA-256 архива и снимок счёта.
Команда фильтрует кандидатов по допуску, свежести, обороту и спреду. Она
отдаёт приоритет открытой позиции со свежим событием и ранжирует остальные
по ROC из stockstats. Вывод содержит `job` для Go Control либо `null`,
если подходящий набор не укладывается в лимит. Владелец указывает остаток
дневных слотов. Сформированное задание несёт собственный durable лимит
вызовов, включая один вызов на распределение портфеля.

```powershell
uv --directory services/research run --offline --frozen python -m tradingbot.selection --root examples/selection --request examples/selection/request.json
```

Пример использует синтетические raw bars в USD. Оборот, bid и ask
должны относиться к заявленной записи. [Схема отбора](contracts/research-selection.schema.json).

### Исследование по устойчивому ID бумаги

Отбор `0.3` связывает тикер с `security_id`, действующим листингом
и источниками данных на момент среза. Отдельный пин владельца задаётся
ссылкой и SHA-256 реестра. В результате отбора `job` версии `0.2`
сохраняет привязки в порядке запрошенных тикеров. Архивный воркер
использует эти же привязки и сохраняет хеш полученного среза в результате
`0.3`. [Пример отбора](services/research/examples/selection/identity-request.json) ·
[Схема реестра](contracts/research-identity-registry.schema.json).

```powershell
$env:TRADINGBOT_RESEARCH_IDENTITY_ARCHIVE_DIR = (Resolve-Path services/research/examples/selection).Path
$env:TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_REF = 'identity-registry.json'
$registryPath = Join-Path $env:TRADINGBOT_RESEARCH_IDENTITY_ARCHIVE_DIR $env:TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_REF
$env:TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256 = (Get-FileHash $registryPath -Algorithm SHA256).Hash.ToLowerInvariant()
docker compose -f deploy/research/compose.yaml --profile research-identity up -d --build postgres control identity-research
```

Передайте те же `--identity-registry-ref` и `--identity-registry-sha256`
при запуске отбора. Для постановки задания используйте свежий снимок
и доступный к нему архив. Исторические даты примера пригодны для локальной
проверки отбора. Воркеры берут задания своей версии и реестра из одной
PostgreSQL. Перед сменой пина завершите уже поставленные задания этого
реестра. Допуск задания `0.2` к виртуальной заявке даёт локальный
планировщик входов владельца.

Для проверки сохранённого среза выгрузите завершённое задание через
`research-replay-export --job`. Передайте SHA-256 выгрузки, корень исходного
архива и пин владельца в `verify-identity-export`. Команда воспроизводит
проекцию архива и сверяет сохранённый хеш без запуска графа.

```powershell
$identityExportPath = (Resolve-Path $identityExport).Path
$identityExportHash = (Get-FileHash $identityExportPath -Algorithm SHA256).Hash.ToLowerInvariant()
uv --directory services/research run --offline --frozen tradingbot verify-identity-export --input $identityExportPath --input-sha256 $identityExportHash --archive-root $env:TRADINGBOT_RESEARCH_IDENTITY_ARCHIVE_DIR --identity-registry-ref $env:TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_REF --identity-registry-sha256 $env:TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256
```

## Локальный офлайн-контур в Docker Compose

Go control, PostgreSQL и Python worker запускаются из корня репозитория.
Control доступен на `127.0.0.1:18080`. PostgreSQL доступна только control по
отдельной внутренней сети. Research работает в сети без внешнего доступа, а
профиль `evaluation` запускает Nautilus без сетевого интерфейса Docker.

```powershell
docker compose -f deploy/research/compose.yaml up -d --build --wait
docker compose -f deploy/research/compose.yaml --profile evaluation run --rm --build evaluation
```

Для автономного запуска создайте виртуальный счёт и задайте его UUID через
`TRADINGBOT_PAPER_ACCOUNT_ID`. Укажите абсолютные пути к существующим
директориям через `TRADINGBOT_PAPER_ARCHIVE_DIR`, `TRADINGBOT_PAPER_CONFIG_DIR`
и `TRADINGBOT_PAPER_QUOTES_DIR`.

- В архиве разместите план входов, ссылки из плана и рыночные снимки.
  `TRADINGBOT_PAPER_ENTRY_PLAN_REF` задаёт путь внутри архива,
  `TRADINGBOT_PAPER_ENTRY_PLAN_SHA256` фиксирует точные байты плана.
- В директории конфига разместите `policy.json` с той же политикой, что и в
  плане входов, и `quote-tape.json` с синтетическими котировками для этого
  счёта. Файл котировок закрепляет `TRADINGBOT_PAPER_QUOTE_TAPE_SHA256`.
- Дайте контейнеру `paper-quote` право записи в директорию котировок
  при UID 65532. Диспетчер открывает эту директорию для чтения.

```powershell
docker compose -f deploy/research/compose.yaml --profile paper --profile paper-entry --profile paper-quote up -d --build postgres control paper-research paper-entry paper-quote paper-dispatch
```

Для пересмотра открытых позиций добавьте `review.json` в конфиг и включите
`paper-review` из профиля `paper`. Журнал планировщика входов хранится в
томе `paper-entry-journal`. После обработки всех окон текущего плана
сервис входов завершается. Для следующего плана укажите его ссылку и хеш
и заново запустите `paper-entry`.

Для paper-входа по устойчивому ID используйте план `0.2` и ленту `0.3`.
План закрепляет SHA-256 реестра, данных отбора и ленты. Тот же реестр
подключите к `identity-research`. Архив с файлами плана доступен через
`TRADINGBOT_PAPER_ARCHIVE_DIR`. Для ленты задайте относительный путь
`TRADINGBOT_PAPER_IDENTITY_QUOTE_TAPE_REF` и SHA-256
`TRADINGBOT_PAPER_QUOTE_TAPE_SHA256`. Диспетчер сверит устойчивый ID с
инструментом из `policy.json` и проверит котировку по исходной ленте.
В этом профиле используются синтетические наблюдения.
[Пример плана](contracts/valid-paper-identity-entry-cycle-plan.json) ·
[Пример ленты](contracts/valid-paper-identity-quote-tape.json).

```powershell
docker compose -f deploy/research/compose.yaml --profile research-identity --profile paper-entry --profile paper-identity up -d --build postgres control identity-research paper-entry paper-identity-quote paper-identity-dispatch
```

Ленту `0.3` можно собрать командой `paper-quote-collect`. Она запрашивает
котировки с фиксированной входной суммой у локального тестового HTTP-сервера.
Команда сверяет направление, mint, входную сумму и срок,
пишет новую ленту для указанного счёта и возвращает её `tape_sha256`.
Для своего счёта укажите его ID и SHA-256 точных байтов реестра в архиве.
Закрепите `tape_sha256` в плане входов и `TRADINGBOT_PAPER_QUOTE_TAPE_SHA256`.
[Схема обмена](contracts/paper-quote-collection.schema.json) ·
[Пример запроса](contracts/valid-paper-quote-collection.json) ·
[Пример ответа сервера](contracts/valid-paper-quote-fixture-response.json).

В примере локальный сервер возвращает приведённый тестовый ответ на
`POST /quote`. Для воспроизведения `--as-of` задаёт момент между доступностью
и истечением тестовой котировки. Без этого флага сборщик проверяет её по
текущему времени. `paper-quote-pin` сверяет доступность котировки после
завершения исследования по времени PostgreSQL. Суммы и комиссии примера
заданы для симуляции.

```powershell
$requestPath = (Resolve-Path contracts/valid-paper-quote-collection.json).Path
$tapePath = Join-Path $PWD 'quote-tape.fixture.json'
go -C services/control run ./cmd/paper-quote-collect --request $requestPath --source http://127.0.0.1:8765/quote --out $tapePath --as-of 2026-11-18T14:31:03Z
```

Для сквозной проверки BUY → SELL задайте `TRADINGBOT_TEST_DATABASE_URL`.
Тест поднимает локальный HTTP-сервер, покупает и затем продаёт всю
виртуальную позицию через `paper`. Он сверяет обе комиссии, закрытие
исходного лота, итоговый баланс и повтор команд без дублей.

```powershell
go -C services/control test -mod=readonly -count=1 -v -run '^TestCollectedFixtureQuoteFlowsThroughStablePaperDispatch$' ./cmd/paper
```

Если в этом же Compose-проекте уже работает сервис `research` для
синтетических smoke jobs, остановите его перед запуском `paper-research`.
Оба читают одну очередь, а paper worker требует архив снимка.

Для отдельной проверки сборки, совместимости, замены и отката worker запустите
`python deploy/research/smoke.py` с Python 3.12 и Docker. Скрипт создаёт временный
Compose-проект, отправляет тестовые задания, проверяет отклонение неподдерживаемой
версии результата, пересоздаёт планировщик между двумя окнами и проверяет
подтверждение HOLD диспетчером. Затем он удаляет свои временные контейнеры
и тома. Скрипт выводит ID образов,
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
пишет виртуальный исход `partially_filled`, `filled`, `unknown` или `cancelled`.
Для частичного исполнения укажите `quantity_raw`. Полное исполнение без
`quantity_raw` закрывает остаток. `unknown` удерживает оставшийся резерв до
сверки. Ответ по ордеру содержит историю `fills` и исполненное и оставшееся
количество. Для журнала используйте
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
[Формат входа и результата](contracts/paper-replay.schema.json).
Котировка 0.2 с `execution: "partial"` указывает `executed_quantity_raw` и
цену всей заявки. Симулятор рассчитывает пропорциональную сумму и комиссию
исполненной части. Неисполненное количество остаётся в резерве до сверки.

Для применения сохранённого research-решения сначала подготовьте policy
владельца и массив наблюдавшихся цен с `at` и `available_at`. Передайте
абсолютные пути к файлам при запуске через `go -C`.

```powershell
go -C services/control run ./cmd/paper snapshot --account $accountId --policy $policyPath --marks $marksPath
go -C services/control run ./cmd/paper apply --policy $policyPath --request $requestPath --quote $quotePath
```

Первая команда выдаёт `portfolio` для research job и `snapshot_context` для
последующего применения. Во второй команде передайте в запросе `job_id`,
`snapshot_context` и `paper_apply_schema_version: "0.1"`. Go выбирает решение
для точного инструмента из сохранённого результата и использует тестовую
котировку, наблюдавшуюся после его записи. При HOLD по всему портфелю
`--quote` можно опустить. Если версия счёта изменилась, следующее новое
исполнение требует свежего снимка и решения. Повтор исходных файлов
возвращает прежний результат по ордеру.
[Пример политики](contracts/valid-paper-policy.json) и [схема команды](contracts/paper-apply.schema.json).

`paper follow` отслеживает результат привязанного задания. Если в
записанном результате все решения HOLD, команда сохраняет квитанцию без
заявки. Для исполнения она читает `<job_id>.json` из явно заданного
`--quote-root`. Этот файл фиксирует путь и SHA-256 локальной котировки.
После резерва PostgreSQL хранит принятые байты. При перезапуске можно
продолжить тот же ордер даже при удалённом исходном файле.

```powershell
go -C services/control run ./cmd/paper follow --request $requestPath --policy $policyPath --quote-root $quoteRoot --poll-interval 5s
```

Файл для `follow` или `dispatch` может выпустить `paper-quote-pin` из
сохранённой синтетической ленты. Для задания пересмотра контекст читается
из durable записи в PostgreSQL. Используется подходящая котировка,
наблюдавшаяся и полученная после завершения задания и ещё не истёкшая.

```powershell
go -C services/control run ./cmd/paper-quote-pin --job $jobId --tape $tapePath --sha256 $tapeHash --quote-root $quoteRoot
```

Для автоматического выбора котировок используйте архив версии `0.2` с
`account_id`. Производитель читает привязки завершённых заданий из PostgreSQL
и ищет подходящее наблюдение по инструменту, стороне и объёму. Для публикации
котировка должна быть firm, поступить после завершения анализа и оставаться
действительной в момент проверки. В режиме опроса новые задания подхватываются
из того же хешированного файла, пока в нём есть подходящие наблюдения.
[Схема ленты](contracts/paper-quote-tape.schema.json) ·
[Пример ленты счёта](contracts/valid-paper-account-quote-tape.json).

```powershell
go -C services/control run ./cmd/paper-quote-pin --account $accountId --tape $tapePath --sha256 $tapeHash --quote-root $quoteRoot --poll-interval 2s
```

Для сравнения синтетических котировок на одной экспозиции запустите
`paper-quote-compare`. Формат фиксирует chain, mint, программу, decimals,
историю коэффициента shares per token и курс settlement к USD. Команда
исключает устаревшие или indicative котировки и при SELL сверяет held mint.
В расчёте к сумме котировки добавляются только внешние комиссии.

```powershell
go -C services/control run ./cmd/paper-quote-compare --input ../../contracts/valid-paper-quote-comparison.json
```

Для серии наблюдений `paper-quote-study` сверяет два хеша. План задаёт
слоты по инструменту, маршруту, стороне, размеру и сессии. Файл наблюдений
записывает полученный ответ или причину отказа. Отчёт отдельно показывает
пропущенные слоты и ответы, которые удалось оценить.

```powershell
$plan = (Resolve-Path contracts/valid-paper-quote-study-plan.json).Path
$observations = (Resolve-Path contracts/valid-paper-quote-study-observations.json).Path
$planPin = (Get-FileHash $plan).Hash.ToLowerInvariant()
$observationsPin = (Get-FileHash $observations).Hash.ToLowerInvariant()
go -C services/control run ./cmd/paper-quote-study --plan $plan --plan-sha256 $planPin --observations $observations --observations-sha256 $observationsPin
```

Для каждого набора расчёт требует заранее заданного покрытия и
статистического минимума. P50, P95 и P99 требуют соответственно 2, 20
и 100 наблюдений. Отчёт применяет метод ближайшего ранга к стоимости
котировки в одну сторону и задержке. Пример использует синтетические
значения. Сведения о реальных котировках, расходах и доступе потребуют
отдельных подтверждений. [Схема исследования](contracts/paper-quote-study.schema.json).

Для заявки `UNKNOWN` или остатка после частичного исполнения
`paper reconcile` принимает отдельное локальное наблюдение симулятора,
привязанное к исходной заявке и котировке. [Процедура сверки](docs/runbooks/paper.md#сверка-зависшей-виртуальной-заявки).

`paper review --account $accountId --order $openingOrderId` показывает
фактический остаток, резерв, ближайший пересмотр и срок тезиса. Время считается
от первого исполнения, включая частичное. Чтобы проверить катализатор из
готового расписания, добавьте `--schedule`, `--archive-root`,
`--event-provider` и `--event-record-id`. Команда возвращает устойчивый ID
положенного пересмотра. [Схема результата пересмотра](contracts/paper-thesis.schema.json).

`schedule-review` сам строит актуальный снимок для открытой позиции и
создаёт задание по сроку или подтверждённому архивному катализатору. Один
триггер получает один job ID. При повторе команда возвращает уже сохранённый
снимок. Укажите абсолютные пути к конфигу и директории архива.

```powershell
go -C services/control run ./cmd/paper schedule-review --config $reviewConfig --archive-root $archiveRoot --poll-interval 30s
```

С `--poll-interval` конфиг перечитывается перед каждым проходом. Передайте
актуальные наблюдавшиеся цены и хеш архива. Без интервала выполняется один
проход. Воркеры читают созданные задания из обычной очереди.
[Пример конфига](contracts/valid-paper-review-schedule.json).

Для позиции, открытой по устойчивому ID, `review` возвращает `opening_identity`
с исходным `security_id`, инструментом и хешем применения. Подготовьте расписание
`0.2` на один момент среза. Команда `prepare-identity-review` читает `review` и
`snapshot` через локальный `paper`, проверяет архив и вычисляет хеш
проекции. Для нового снимка обновите вход и сохраните принятый конфиг вместе с
хешированными файлами для повтора.

```powershell
$paperCLI = Join-Path ([System.IO.Path]::GetTempPath()) ('tradingbot-paper-' + [Guid]::NewGuid() + '.exe')
go -C services/control build -mod=readonly -o $paperCLI ./cmd/paper
uv --directory services/research run --offline --frozen tradingbot prepare-identity-review --paper-cli $paperCLI --archive-root $archiveRoot --account $accountId --order $openingOrderId --as-of $cutoff --policy $policyPath --marks $marksPath --data-manifest-ref $manifestRef --data-manifest-sha256 $manifestHash --identity-registry-ref $registryRef --identity-registry-sha256 $registryHash --quote-tape-ref $quoteTapeRef --quote-tape-sha256 $quoteTapeHash --max-llm-calls 32 > $reviewConfig
& $paperCLI schedule-review --config $reviewConfig --archive-root $archiveRoot --identity-registry-sha256 $registryHash
```

Передавайте время `$cutoff`, для которого цены и состояние счёта были доступны.
Go возвращает `not_due` до положенного пересмотра. Для подтверждённого
архивного катализатора передайте `event` в конфиге `0.2` по схеме команды.
В Compose `paper-review` читает тот же конфиг из `review.json` и использует
`TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256`. Задание `0.2` получает воркер
`identity-research` с тем же пином. Для виртуального применения работают
`paper-identity-quote` и `paper-identity-dispatch`.

Для автоматического пересмотра подготовьте [план окон](contracts/paper-review-cycle-plan.schema.json).
[Синтетический пример](contracts/valid-paper-identity-review-cycle-plan.json) показывает привязку к открытию и архивному событию.
Каждое окно задаёт ID открытия, срез, срок запуска, хеши архивов и лимит
вызовов. На наступившем окне `paper-review-cycles` получает состояние счёта,
проверяет проекцию и сохраняет точные байты запроса до передачи в Go.

```powershell
$env:TRADINGBOT_PAPER_REVIEW_PLAN_REF = 'review-plan.json'
$env:TRADINGBOT_PAPER_REVIEW_PLAN_SHA256 = (Get-FileHash (Join-Path $env:TRADINGBOT_PAPER_ARCHIVE_DIR $env:TRADINGBOT_PAPER_REVIEW_PLAN_REF)).Hash.ToLowerInvariant()
$env:TRADINGBOT_RESEARCH_IDENTITY_ARCHIVE_DIR = $env:TRADINGBOT_PAPER_ARCHIVE_DIR
docker compose -f deploy/research/compose.yaml --profile research-identity --profile paper-review-cycles up -d --build postgres control identity-research paper-review-cycles
```

Укажите пины реестра `TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_REF` и
`TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256` из того же архива.
Запуск из одного плана использует том `paper-review-journal`. После рестарта
сохранённый запрос отправляется с прежним снимком. Если следующий срез
приходится на уже принятый триггер, Go возвращает ID из PostgreSQL.
Для нового плана используйте отдельный журнал. При локальном запуске
`python -m tradingbot.review_cycles` путь задаёт `--journal`.
Планировщик использует локально подготовленные цены и события. Чтобы
продолжить виртуальное исполнение, запустите профиль `paper-identity`.

Для первого входа `paper schedule-entry` принимает готовый результат
отбора и конфиг владельца со снимком счёта, временем запуска, лимитом
вызовов и SHA-256 точных байтов отбора. Go перепроверяет состояние счёта,
разрешённые бумаги и архив. Одно расписание на один момент создаёт одно
задание и сохраняет контекст применения в той же транзакции.

```powershell
go -C services/control run ./cmd/paper schedule-entry --config $entryConfig --selection $selectionPath --archive-root $archiveRoot
```

[Контракт расписания входа](contracts/paper-apply.schema.json#/$defs/EntryScheduleRequest) · [Пример расписания](contracts/valid-paper-entry-schedule.json).

Для нескольких заранее заданных срезов `tradingbot.entry_cycles` принимает
хешированный план. Он собирает доступный снимок, запускает отбор и сохраняет
точные входы Go в локальном журнале до отправки задания. При повторе
используются записанные байты. Просроченный срез без записи пропускается.

[Профиль на 23 бумаги](services/research/examples/entry-cycles/expanded-screening.json)
показывает широкий отбор с синтетическими ценами базовых бумаг и устойчивыми ID.
При `top_k=3`, бюджете 33 вызова и 16 вызовах на бумагу в исследование
попадают максимум две бумаги за запуск. Для своего paper-плана `0.2`
в `occurrences[].screening` укажите профиль, закрепите реестр в
`identity_registry` и подготовьте рыночный архив, marks и ленту котировок
на нужный срез. В примере пути к архиву и реестру заданы от корня репозитория.
[Состав списка и критерии](docs/planning/plan.md#9-universe-selection)

```powershell
uv --directory services/research run --offline --frozen python -m tradingbot.entry_cycles --root $ownerRoot --plan-ref $cyclePlanRef --plan-sha256 $cyclePlanHash --journal $entryJournal --paper-cli $paperCLI
```

Передайте абсолютные пути к корню, постоянному журналу и собранному
`paper`. План содержит относительные ссылки из этого корня. Чтобы
процесс ожидал очередных окон, добавьте `--poll-interval-seconds 1`.
В примере даты исторические. [Схема плана](contracts/paper-entry-cycle-plan.schema.json) · [Пример плана](contracts/valid-paper-entry-cycle-plan.json).

Новые задания пересмотра получают durable запись для `dispatch` в той же
транзакции. Диспетчер выбирает завершённые задания заданного счёта,
сопоставляет policy владельца и выполняет `follow`. Подтверждение наступает
после записи результата применения. Если процесс сбился между этими шагами,
следующий проход читает ту же квитанцию. Без готовой котировки заявка
остаётся невыполненной. После смены политики диспетчер может завершить уже
сохранённое применение по его исходной привязке и закреплённой котировке.
Новые резервы проходят по действующей политике.

```powershell
go -C services/control run ./cmd/paper dispatch --account $accountId --policy $policyPath --quote-root $quoteRoot --poll-interval 5s
```

Чтобы передать фактические исходы в следующее исследование, экспортируйте
их из paper ledger к заданному `as_of`. Команда добавит в новый архив
ограниченное число реализованных исходов с ID исполнения и точными суммами.

```powershell
go -C services/control run ./cmd/paper-memory --account $accountId --as-of $snapshotAsOf --policy $policyPath --input $marketArchivePath --input-sha256 $marketArchiveHash --output $newArchivePath
```

Журнал уроков фиксирует результат конкретного виртуального SELL по FIFO
и связывает его с событием исполнения, решением и версией счёта.
`record-lesson` получает суммы из ledger. `correct-lesson` добавляет событие
отзыва или восстановления, сохраняя ранее записанные суммы и даты. Для
повтора передавайте тот же UUID операции. `lessons` показывает ревизию,
доступную к указанному `as_of`. Ключ `--history` выводит до 100 видимых
событий выбранного урока.

```powershell
go -C services/control run ./cmd/paper-memory record-lesson --account $accountId --fill-event $sellFillEventId --event $recordEventId
go -C services/control run ./cmd/paper-memory correct-lesson --account $accountId --lesson $lessonId --event $correctionEventId --expected-revision 1 --action retract
go -C services/control run ./cmd/paper-memory lessons --account $accountId --lesson $lessonId --as-of $snapshotAsOf --history
```

[Контракт события журнала уроков](contracts/paper-memory-lesson.schema.json).

Добавьте `--durable-lessons` к команде экспорта архива, чтобы использовать
уроки из журнала. Архив связывает отражение с активной ревизией на момент
`as_of`. Исторический прогон добавляет его в режиме `factual_reflection`
на том же срезе. После отзыва в новых срезах остаются исходные суммы FIFO.

`paper-thesis-outcome` оценивает конкретное открытие по FIFO. Предел
горизонта считается от первого записанного исполнения по максимальному
сроку из решения. Для открытого остатка нужен полный по размеру,
свежий и ещё действовавший к сроку синтетический SELL. Если
его нет, оценка остатка остаётся неизвестной. Продажи, записанные позже,
меняют фактический результат к `as_of`, сохраняя исход на горизонте.

```powershell
go -C services/control run ./cmd/paper-thesis-outcome --input $outcomeInput --sha256 $outcomeHash
```

[Схема отчёта](contracts/paper-thesis-outcome.schema.json) · [Пример входа](contracts/valid-paper-thesis-outcome-input.json).

Для открытия с устойчивым ID используйте вход `0.2` с пустым `exit_observations`.
При оценке оставшейся позиции передайте `exit_tape` с `ref` и `sha256` ленты
`0.3`, `exit_identity_registry` с пином её реестра и укажите `--archive-root`.
Лента должна относиться к тому же виртуальному счёту. Реестр может обновиться,
если исходная бумага и точный инструмент допущены в момент котировки и на
горизонте. Отчёт сохраняет хеши открытия, ленты, реестра и выбранной котировки.
Для реализованных сумм он проверяет сохранённое применение
каждой отнесённой FIFO продажи. Если продажа не имеет проверяемой привязки к
той же бумаге, команда возвращает ошибку.

Для отклонённого симулятором открытия `paper-shadow-outcome` проверяет
сохранённую квитанцию и событие отмены. Два отдельных файла фиксируют цены,
время получения и комиссии гипотетического BUY и SELL. Записи без цены в
нужном окне остаются без оценки. Отчёт только читает счёт, а результат
помечается как контрфактуальный.

```powershell
go -C services/control run ./cmd/paper-shadow-outcome --input $shadowInput --sha256 $shadowHash --entry $entryObservation --exit $exitObservation
```

[Контракт контрфактуальной оценки](contracts/paper-shadow-outcome.schema.json).

Запишите новый файл в корень архива worker и используйте напечатанный
`{ref,sha256}` как `data_manifest` задания. Paper-исходы идут отдельным
контекстом, в котором учтены зафиксированные комиссии. Бенчмарк и OPEX
в эти исходы не входят. [Схема исхода](contracts/paper-outcome.schema.json).

Для создания заданий по готовому расписанию запустите `go -C services/control run
./cmd/research-scheduler --once --config <schedule.json> --archive-root <путь>`.
Планировщик проверяет хеш архива и вычисляет ID из расписания и времени события.
Повтор того же запуска возвращает существующее задание. Для собственного прогона
укажите актуальные `scheduled_at` и `portfolio.as_of` и вычислите SHA-256 текущих
байтов архива. Формат показан в [примере расписания](contracts/valid-research-schedule.json).
Версия 0.2 также принимает ссылку на событие из архива. Событие должно уже быть
известно и загружено к `portfolio.as_of`. ID задания для события привязан к имени
расписания, источнику и ID записи, даже если время запуска отредактировали.
[Пример событийного расписания](contracts/valid-research-event-schedule.json).

При запуске после пропуска Go сохраняет в PostgreSQL наблюдение о явно
заданном календарном окне, закрытом не более 24 часов назад. Для проверки
возьмите `job_id` из лога планировщика и прочитайте запись через Control.

```powershell
Invoke-RestMethod "http://127.0.0.1:18080/v1/research/schedule-occurrences/$researchJobId"
```

Статус `missed` означает отсутствие задания с этим ID на момент чтения.
`job_recorded` возвращает время сохранённого задания с тем же хешем запроса.
`job_conflict` показывает занятой ID с другим хешем. Повтор сохраняет время
первого наблюдения. Это ограниченный журнал календарной конфигурации и
закрытых окон, встреченных планировщиком. Архивные катализаторы в него не входят.
[Схема](contracts/research-calendar-occurrence.schema.json) · [Пример записи](contracts/valid-research-calendar-occurrence.json).

Для просмотра целого календаря сохраните исходный файл расписания и его
SHA-256. `research-schedule-coverage` читает PostgreSQL одним снимком и
группирует заявленные окна по UTC. По каждому окну видны время поступления
задания, число попыток, разрешения и записанные ответы. У закрытого окна
отдельно показаны зарегистрированный пропуск, отсутствие сведений и
конфликт сохранённого запроса. Пустые дни и событийные задания находятся
вне отчёта. [Схема покрытия](contracts/research-schedule-coverage.schema.json).

```powershell
go -C services/control run ./cmd/research-schedule-coverage --config $schedulePath --sha256 $scheduleHash
```

Независимый Nautilus replay читает подготовленные тестовые котировки и заявки.
Денежные дивиденды учитываются по времени права и выплаты. Сплиты и обратные
сплиты меняют количество на границе события, сохраняя исходные цены сделок.
Вычисления требуют точного представления в сетке Nautilus для исходных акций
и центов USD. `benchmarks` на том же горизонте сравнивает cash, buy-and-hold
и равную начальную аллокацию. Дополнительный трендовый бенчмарк использует
прошлые доступные midpoints, деньги счёта и следующую подходящую котировку.
Порог длительности пропуска и длина истории задаются до прогона.

Равновзвешенный бенчмарк с ребалансировкой принимает один или несколько
UTC-моментов через `--rebalance-at`. `--rebalance-phase-seconds` задаёт
длительность окна продаж и следующего окна покупок, а
`--rebalance-max-mark-age-seconds` задаёт допустимый возраст цены.
Размер покупок определяется после окна продаж по фактическому свободному
кэшу симулятора. В отчёте сохраняются время расчёта, состав начального
universe, исполненные заявки, свободный кэш и комиссии.
Для дневной оценки зафиксируйте эталон `kind: "equal_weight_scheduled"`
и объект `rebalance_schedule` с `rebalance_at`, `execution_phase_seconds`
и `max_mark_age_seconds` в протоколе. `freeze-statistics` включает
расписание в хеш протокола. [Схема протокола](contracts/evaluation-protocol.schema.json).

```powershell
uv --directory services/evaluation run --frozen tradingbot-evaluation replay examples/quote-replay.json
uv --directory services/evaluation run --frozen tradingbot-evaluation benchmarks examples/quote-replay.json --start-at 2026-10-02T13:30:10Z --buy-and-hold-symbol TSLA
uv --directory services/evaluation run --frozen tradingbot-evaluation benchmarks examples/quote-replay.json --start-at 2026-10-02T13:30:10Z --buy-and-hold-symbol TSLA --trend-symbol TSLA --trend-lookback-quotes 1 --trend-max-gap-seconds 60
```

Для проверки сессий включите `execution_calendar` в синтетическую ленту
до расчёта её хеша. Для каждого тикера укажите непересекающиеся окна
`open`, `closed` или `halted`, ID записи, начало, исключённую верхнюю
границу и время доступности этой записи. Время наблюдения и поступления
котировки должно попасть в одно открытое окно, уже известное к моменту
поступления. Следующее окно требует нового наблюдения. Просроченная
заявка истекает по своему сроку. Одни и те же окна применяются к
исполнению, оценке позиций и бенчмаркам. Отчёт показывает причины
пропуска котировок и ID окон исполнений и оценок. Доступность реальной
площадки требует отдельной проверки.
[Вход календарного прогона](contracts/valid-evaluation-replay-calendar.json) · [Схема](contracts/evaluation-replay.schema.json).

Для прогона сохранённых решений экспортируйте завершённые research jobs из
PostgreSQL. `research-replay` принимает хеши экспорта и отдельной синтетической
ленты, у которой `orders` пуст. В отчёте видны исходные ID и статусы решений.
Для поддерживаемых заявок симулятор использует целые акции и первую подходящую
котировку после записи результата. Исполнения используют общий синтетический
счёт. Ключ `--require-snapshot-continuity` сверяет все исходные снимки
с состоянием этого счёта к их времени. Проверяются cash, нулевой денежный
резерв, точные инструменты и количества, а также midpoints удерживаемых
бумаг. Проверка использует только котировки и события, доступные к
соответствующему срезу. Если исполнение другой заявки или корпоративное
действие меняет счёт до планируемого исполнения, строгий прогон сообщает
об устаревшем снимке.

```powershell
# Перенаправление байтов stdout поддерживается в PowerShell 7.4+.
go -C services/control run ./cmd/research-replay-export --job $jobId > $exportPath
$exportHash = (Get-FileHash $exportPath).Hash.ToLowerInvariant()
$tapeHash = (Get-FileHash $tapePath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation research-replay $exportPath $tapePath --export-sha256 $exportHash --tape-sha256 $tapeHash --require-snapshot-continuity
```

Для дневного сравнения сохраните экспорт, ленту и их хеши вместе с отчётом.
Ключ `--emit-replay-input` выводит производный вход для `replay-evidence`.
Сохраните его отдельно и укажите `normalized_input_sha256` из отчёта
в замороженном протоколе. Для воспроизведения конечного остатка обычным
`replay` передайте значение `evaluated_through` из отчёта через
`--through`. [Схема экспорта](contracts/research-replay-export.schema.json).

Для сохранённых решений `0.2` используйте ленту с реестром и отдельным
ключом Nautilus на каждую бумагу. Передайте дополнительно `--identity-registry`
с путём к файлу и `--identity-registry-sha256` с пином владельца. Сверка
остатков, позиций и отметок включается автоматически. Все задания в таком
прогоне должны ссылаться на те же байты реестра. Ключ `--emit-replay-input`
сохраняет привязки вместе с нативными заявками. [Схема ленты](contracts/evaluation-identity-replay.schema.json).

Для дневного сравнения сохранённых решений используйте `replay_source` в
протоколе `0.2`. Поле `replay_sha256` берётся из `normalized_input_sha256`
отчёта `research-replay`. В `replay_source.identity` укажите
`research_export_sha256`, `tape_sha256`, `identity_registry_sha256` и
`mark_max_age_seconds`. Для эталона из одной бумаги укажите `security_id`.
Стратегия и бенчмарк используют одинаковые даты UTC, начиная с контрольной
даты с начальным капиталом до первой заявки.

```powershell
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation replay-evidence $protocolPath $tapePath --protocol-sha256 $protocolHash --research-export $exportPath --identity-registry $registryPath > $evidencePath
```

Команда заново проверяет все исходные хеши, перестраивает заявки и считает
дневной NAV с учётом спреда и комиссий симулятора. Для статистики передайте
хеш сохранённого отчёта. Издержки на данные, инфраструктуру и модели
остаются неизвестными в синтетическом прогоне.

`walk-forward` формирует снимок из состояния Nautilus к каждому сроку
в плане и запрашивает новое решение у закреплённого офлайн-графа TradingAgents
в отдельном Python-процессе. Архив передаёт только записи, полученные к
срезу. Время окончания анализа задано синтетическим планом. До первого
запуска проверяется общий лимит вызовов всех шагов, а в отчёте сохраняются
снимки, хеши запросов и результатов, расход и исход каждого решения.

```powershell
$walkPlan = (Resolve-Path contracts/valid-walk-forward-plan.json).Path
$walkTape = (Resolve-Path services/evaluation/examples/walk-forward-tape.json).Path
$walkArchive = (Resolve-Path services/research/examples/selection).Path
$walkPython = uv --directory services/research run --offline --frozen python -c "import sys; print(sys.executable)"
$walkPlanHash = (Get-FileHash $walkPlan).Hash.ToLowerInvariant()
$walkTapeHash = (Get-FileHash $walkTape).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation walk-forward $walkPlan $walkTape --research-python $walkPython --archive-root $walkArchive --plan-sha256 $walkPlanHash --tape-sha256 $walkTapeHash
```

Ключ `--include-replay-input` добавляет производный вход для
`replay-evidence`. Офлайн-модель в этом примере проверяет конвейер на
синтетических данных и возвращает HOLD. [Схема плана](contracts/walk-forward-plan.schema.json).

Для прогона с устойчивыми ID возьмите [план 0.2](services/evaluation/examples/identity-walk-plan.json)
и [ленту 0.2](services/evaluation/examples/identity-walk-tape.json). Укажите их пути
в предыдущей команде, пересчитайте хеши и добавьте
`--identity-registry $walkRegistry --identity-registry-sha256 $walkRegistryHash`.
Файл `$walkRegistry` лежит в `$walkArchive` по пути `identity-registry.json`.
Nautilus ведёт позиции по отдельным ключам бумаг. В каждый снимок research
попадают их точные инструменты и доступные цены. Прогон прерывается, если
оставшуюся позицию невозможно однозначно оценить на финальном горизонте.

Чтобы воспроизвести дневной отчёт, используйте `walk-forward-evidence`.
Протокол фиксирует план, ленту, даты и бенчмарки до запуска. Команда
выполняет анализ по каждому срезу, пересчитывает NAV и сохраняет хеш шагов
и производного replay. Список дат должен покрывать все сутки UTC,
а позиции стратегии и бенчмарка требуют отметок в пределах указанного
возраста. `statistics` сверяет происхождение отчёта с протоколом. Для
проверки сохранённого файла передайте независимо записанный хеш через
`--evidence-sha256`. [Пример протокола](services/evaluation/examples/walk-forward-protocol.json).

```powershell
$walkProtocol = (Resolve-Path services/evaluation/examples/walk-forward-protocol.json).Path
$walkProtocolHash = (uv --directory services/evaluation run --offline --frozen tradingbot-evaluation freeze-statistics $walkProtocol | ConvertFrom-Json).protocol_sha256
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation walk-forward-evidence $walkProtocol $walkPlan $walkTape --research-python $walkPython --archive-root $walkArchive --protocol-sha256 $walkProtocolHash > $walkEvidencePath
$walkEvidenceHash = (Get-FileHash $walkEvidencePath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation statistics $walkProtocol $walkEvidencePath --protocol-sha256 $walkProtocolHash --evidence-sha256 $walkEvidenceHash
```

С [протоколом для устойчивых ID](services/evaluation/examples/identity-walk-protocol.json)
передайте план и ленту `0.2`, добавив к `walk-forward-evidence`
`--identity-registry $walkRegistry`. Хеш `walk_forward_source.identity_registry_sha256`
должен совпасть с планом и лентой до первого анализа. Для эталона из одной
бумаги протокол задаёт `security_id`. `statistics` сверяет хеш реестра в отчёте.

Для сравнения конфигураций `ablation-evidence` выполняет все варианты
из одного протокола на общих датах, исходном капитале, архиве и ленте.
У каждого варианта свой виртуальный портфель. Общий лимит проверяется
до запуска, а отчёт содержит расход вызовов, хеши шагов и конфигураций.
В примере сравниваются полный граф, граф без памяти, один аналитик
и граф с производной меткой фактического FIFO-исхода.
[Протокол](services/evaluation/examples/ablation-protocol.json) · [План](contracts/valid-ablation-walk-plan.json).

```powershell
$ablationProtocol = (Resolve-Path services/evaluation/examples/ablation-protocol.json).Path
$ablationPlan = (Resolve-Path contracts/valid-ablation-walk-plan.json).Path
$ablationArchive = (Resolve-Path services/research/examples/paper-reflection).Path
$ablationPin = (uv --directory services/evaluation run --offline --frozen tradingbot-evaluation freeze-statistics $ablationProtocol | ConvertFrom-Json).protocol_sha256
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation ablation-evidence $ablationProtocol $ablationPlan $walkTape --research-python $walkPython --archive-root $ablationArchive --protocol-sha256 $ablationPin > $ablationEvidencePath
$ablationEvidenceHash = (Get-FileHash $ablationEvidencePath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation statistics $ablationProtocol $ablationEvidencePath --protocol-sha256 $ablationPin --evidence-sha256 $ablationEvidenceHash
```

Для плана и ленты `0.2` добавьте `ablation_source.identity_registry_sha256`
в протокол `0.2` и передайте `--identity-registry $walkRegistry`. Все варианты
используют те же байты реестра, а `statistics` сверяет его хеш в каждом варианте.

Для forward-эксперимента перечислите в плане файлы запуска стратегии и
каждого альтернативного варианта. Первая команда вычисляет
`configuration_sha256` для протокола оценки. После заполнения этого поля
сохраните результат `freeze-forward` и его SHA-256 отдельно от рабочих
файлов. `verify-forward` повторно проверяет содержимое файлов, состав
сравнений и протокол по сохранённому хешу. Время регистрации подтверждается
внешней записью.

```powershell
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation forward-config-sha256 $forwardPlan --root $workspace
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation freeze-forward $forwardPlan $protocolPath --root $workspace > $freezePath
$freezeHash = (Get-FileHash $freezePath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation verify-forward $freezePath $protocolPath --root $workspace --freeze-sha256 $freezeHash
```

[Пример локального плана](contracts/valid-forward-plan.json) · [Схема фиксации](contracts/forward-freeze.schema.json).

В forward paper до начала запишите в `paper_forward_source` ID счёта,
стартовый капитал, источник и допустимый возраст цен, контрольные даты UTC,
порог просадки и настройки дополнительного эталона, если он есть. Календарь
охватывает все сутки. Хеши конкретных выгрузок сохраняются отдельно при
подготовке отчёта за контрольный период. Следующий пример использует
синтетический ряд и тот же `valid-forward-plan.json`.

```powershell
$workspace = (Get-Location).Path
$forwardPlan = (Resolve-Path contracts/valid-forward-plan.json).Path
$trialDir = Join-Path ([IO.Path]::GetTempPath()) ('tradingbot-forward-' + [guid]::NewGuid())
[void][IO.Directory]::CreateDirectory($trialDir)
$protocolPath = Join-Path $trialDir 'protocol.json'
$freezePath = Join-Path $trialDir 'freeze.json'
$config = uv --directory services/evaluation run --offline --frozen tradingbot-evaluation forward-config-sha256 $forwardPlan --root $workspace | ConvertFrom-Json
$protocol = Get-Content -Raw services/evaluation/examples/forward-paper-protocol.json | ConvertFrom-Json
$protocol.configuration_sha256 = $config.configuration_sha256
[IO.File]::WriteAllText($protocolPath, ($protocol | ConvertTo-Json -Depth 20))
$freezeJson = uv --directory services/evaluation run --offline --frozen tradingbot-evaluation freeze-forward $forwardPlan $protocolPath --root $workspace
[IO.File]::WriteAllText($freezePath, $freezeJson)
$freezeHash = (Get-FileHash $freezePath).Hash.ToLowerInvariant()

$navPath = (Resolve-Path services/evaluation/examples/paper-cash-nav.json).Path
$navHash = (Get-FileHash $navPath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation forward-paper-report $freezePath $protocolPath --root $workspace --freeze-sha256 $freezeHash --through 2026-10-05 --nav $navPath --nav-sha256 $navHash
```

Для нового отчёта передайте выгрузку `paper-nav` со всеми датами от
пополнения до указанного дня включительно. При регистрации `operating_net`
передайте `--operating-costs` и `--operating-costs-sha256` вместе. Пока
расходы отсутствуют или неполны, для сравнения доступен только `execution_net`.
Для индекса полной доходности передайте `--benchmark` и
`--benchmark-sha256`. Для локального архива используйте `retrospective`
в протоколе, `local_archive` в файле ряда и одинаковый
`archive_source_id` в регистрации индекса и в файле. Укажите
`theoretical_reference` или `unknown` для доступа к индексу. Оценка
сверяет SHA-256, идентичность источника и ряда, покрытие всех
зарегистрированных суток и время доступности значений. Доступность
индекса владельцу, права использования данных и рыночный результат
требуют отдельных подтверждений.
Промежуточный отчёт описывает накопленную выборку. Статус порога доходности
появляется на финальной дате. Просадка сверяется с
порогом от первого дня. Хеши подтверждают содержимое файлов. Для подтверждения
времени регистрации и происхождения наблюдений нужны независимые записи.

Для связанного ряда отчётов до регистрации задайте
`paper_forward_source.checkpoint_history = "require_prior_receipt"`.
Первый контрольный отчёт содержит `checkpoint_receipt`. Сохраните этот
объект в отдельном JSON и отдельно сохраните его SHA-256. Для следующего
отчёта добавьте `--previous-receipt <файл> --previous-receipt-sha256 <хеш>`.
Команда сверит непосредственно предыдущую зарегистрированную дату.
Уже опубликованные строки NAV, значения индекса и записи расходов
должны совпасть. Новые сутки добавляются после старых, а статус покрытия
расходов относится ко всему новому периоду. Если предыдущий отчёт
не содержал файла расходов, у него ещё нет зафиксированного ряда
расходов. [Схема квитанции](contracts/forward-checkpoint-receipt.schema.json).

`execution_cost_diagnostics` показывает дневной NAV до явно проведённых
комиссий исполнений. Расчёт использует проведённые объёмы и цены исполнения,
а также дневные отметки. Полный gross NAV помечен как недоступный. Для его
расчёта нужны независимые ориентиры цены на момент исполнения и согласованная
база дневной оценки.

Поле `paper_forward_source.power_review` фиксирует эффект над минимальным
дневным превышением эталона, предполагаемое стандартное отклонение и
корреляцию соседних дневных доходностей, целевую вероятность обнаружения,
число испытаний и seed симуляции. Команда проверяет проект исследования
до получения NAV.

```powershell
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation forward-paper-power $protocolPath
```

Метод `gaussian_ar1_paired_moving_block_percentile_v1` симулирует правило
нижней границы блочного интервала и показывает долю пересечений при нулевом
эффекте и при заданном эффекте. Интервалы Вильсона отражают погрешность
симуляции. Каждый контрольный отчёт повторяет расчёт для зарегистрированного
финального календаря. В пятидневном примере недостаточно блоков.
[Методология блочного bootstrap](https://doi.org/10.1214/aos/1176347265) · [Дизайн симуляций](https://doi.org/10.1002/sim.8086).

В новой регистрации можно выбрать `gaussian_ar1_paired_mean_v1`. Этот метод
использует нормальный интервал для среднего дневного превышения, точную
конечновыборочную дисперсию стационарной модели AR(1) и поправку Бонферрони.
Волатильность и корреляция взяты из протокола. `forward-paper-power` сверяет
аналитические вероятности с независимыми симуляциями нулевого и заданного
эффектов. `model_conditional_inference` появляется с данными только на финальном
чекпойнте и в зарегистрированной базе NAV. Блочный интервал остаётся в
`comparisons`. Рыночная применимость допущений и прохождение G5 неизвестны.
[Ковариация AR(1)](https://www.stat.cmu.edu/~cshalizi/dst/18/lectures/15/lecture-15.html) · [Поправка Бонферрони](https://doi.org/10.1080/01621459.1961.10482090).

Настройки `paper_forward_source.regime_review` задаются до регистрации
эксперимента, в котором основным эталоном служит индекс полной доходности.

```json
{
  "lookback_utc_days": 10,
  "minimum_distinct_observations": 6,
  "rising_threshold_bps": 100,
  "declining_threshold_bps": 100,
  "minimum_pairs_per_regime": 5
}
```

Для каждой пары дневных доходностей режим рассчитывается по индексным
уровням, доступным к началу этой пары. Повтор отметки в выходные
сохраняет дневную пару и учитывается как одно индексное наблюдение.
Отчёт сохраняет все пары, включая неклассифицированные. По каждой
группе видны число пар и среднее
превышение в доступной базе NAV. Порог покрытия и средние по группам
используются как описательные показатели. Достаточность рыночной
выборки проверяется отдельно.

Для проверки протокола сравнения `freeze-statistics` выводит SHA-256
заранее зарегистрированных периодов, бенчмарков и параметров блочного bootstrap.
Команда `statistics` сверяет этот хеш, даты NAV и доступность входов. Пример
формирует отчёт на синтетическом наборе.

```powershell
uv --directory services/evaluation run --frozen tradingbot-evaluation freeze-statistics examples/statistics-protocol.json
uv --directory services/evaluation run --frozen tradingbot-evaluation statistics examples/statistics-protocol.json examples/statistics-evidence.json --protocol-sha256 b05ddb0e27ed22643300a46ede0df85e33a4d795d3eb9cf95730e8fa1b5f1b39
```

`replay-evidence` строит вход для `statistics` непосредственно из нативного
Nautilus replay. Используйте пару `examples/replay-evidence-protocol.json` и
`examples/replay-evidence-quotes.json` в том же окружении. Передайте хеш из
`freeze-statistics` через `--protocol-sha256` и сохраните JSON-вывод. Дневные
NAV учитывают симулированный спред и комиссии. Внешние расходы неизвестны.

Дневной NAV виртуального счёта экспортирует `paper-nav`. Хеш входного
файла фиксирует точные наблюдавшиеся цены и допустимый возраст. Если
какая-либо позиция не оценена, вывод сохраняет дату и причину, а команда
возвращает код 1. Для сопоставимого ряда `paper-evidence` сверяет хеш отчёта,
полный календарь UTC, cash и оценку позиций и добавляет эталон
`uninvested-cash` с тем же стартовым капиталом.

```powershell
go -C services/control run ./cmd/paper-nav --input $paperNavInput --sha256 $inputHash
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation paper-evidence examples/paper-cash-protocol.json examples/paper-cash-nav.json --protocol-sha256 811c97f1b0d0c36d9473ab8612af964d6e2e8fc1d759728e5d46c89db35e417f
```

Перед использованием сохранённого отчёта его можно воспроизвести по тому
же входному файлу цен и PostgreSQL. Команда читает журнал в одном снимке
и выдаёт пересчитанный NAV только при совпадении. Совпавший отчёт
с пропущенной ценой сохраняет код 1.

```powershell
go -C services/control run ./cmd/paper-nav --input $paperNavInput --sha256 $inputHash --verify-report $navPath --report-sha256 $navHash
```

Для синтетического сравнения с индексом полной доходности передайте
`--benchmark-input`. Протокол фиксирует хеш ряда, а его файл сообщает
время получения каждого значения и модельный расход на вход.
Пропущенная дата, будущая или устаревшая оценка останавливают расчёт.
[Пример протокола](services/evaluation/examples/paper-total-return-protocol.json) · [Схема бенчмарка](contracts/paper-benchmark.schema.json).

Для `operating_net` используйте протокол 0.2 с хешем cost ledger и
передайте `--operating-costs` в `statistics`. Команда учитывает отнесённые
расходы после построения execution-net NAV. Прежние спред и комиссия
Nautilus уже входят в этот ряд. Если часть расходов неизвестна,
доступен только execution-net. Обе панели содержат просадку и риск
за наблюдаемый интервал.
[Пример протокола](services/evaluation/examples/operating-costs-protocol.json) · [Схема затрат](contracts/evaluation-operating-costs.schema.json).

Для локального журнала вызовов добавьте `--usage-output <путь к новому файлу>`
к `historical-step` или `analyze`. В журнал входят ID, роль, начало и результат
каждого вызова, принятого в рамках лимита. Счётчики без данных остаются
`null`. Остановка анализа через обработанную ошибку оставляет `partial`.
При неоднозначном endpoint или тарифе поля модели и провайдера пусты.
Команда ожидает новый файл в существующем каталоге.

```powershell
$archive = (Resolve-Path services/research/examples/selection).Path
$usagePath = Join-Path ([IO.Path]::GetTempPath()) ('tradingbot-usage-' + [guid]::NewGuid() + '.json')
Get-Content -Raw contracts/valid-research-historical-step.json | uv --directory services/research run --offline --frozen tradingbot historical-step --archive-root $archive --usage-output $usagePath
```

Скриптовая модель сохраняет число вызовов. Для примера расчёта ниже
используется синтетический журнал с заданными счётчиками.

```powershell
$ratesPath = (Resolve-Path services/evaluation/examples/research-rate-schedule.json).Path
$usagePath = (Resolve-Path contracts/valid-research-llm-usage.json).Path
$ratesPin = (Get-FileHash $ratesPath).Hash.ToLowerInvariant()
$usagePin = (Get-FileHash $usagePath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation research-costs --rates $ratesPath $ratesPin --usage $usagePath $usagePin
```

`research-costs` выбирает ставку по провайдеру, модели и дате UTC.
Токены чтения и записи кеша входят во входной счётчик. Reasoning входит
в выходной. Отчёт применяет отдельные ставки к этим долям и сохраняет
известную часть расходов при неполном журнале. P95 описывает только
переданные архивы, для которых удалось оценить все вызовы. Фактическая
сумма к оплате остаётся неизвестной.

Для расчёта сценарного `operating_net` передайте
`--synthetic-opex-template <файл затрат> <хеш> --series <имя ряда NAV>`.
Исходный файл содержит остальные статьи затрат. Команда добавляет
дневные модельные затраты на LLM, округлённые вверх до восьми знаков.
Перенесите поле `synthetic_operating_costs` в отдельный JSON и используйте
его как журнал затрат. Повторное добавление LLM к тому же ряду приводит
к ошибке. [Схема журнала](contracts/research-llm-usage.schema.json) · [Схема ставок](contracts/research-model-rates.schema.json).

Для заданий worker используется журнал разрешений PostgreSQL. Каждый
допущенный вызов занимает слот. В конце вызова worker сохраняет наблюдение
с тем же ID. После аварии разрешение без ответа остаётся занятым и
отображается как неизвестный расход в отчёте. Чтение включает прежние
попытки, даже если итоговый результат пришёл от нового процесса.
При локальной замене сначала обновите Control. Worker проверяет поддержку
записи вызовов до получения задания.

```powershell
# TRADINGBOT_DATABASE_URL указывает на локальную PostgreSQL
go -C services/control run ./cmd/research-usage-export --job $researchJobId > $permitArchivePath
$permitPin = (Get-FileHash $permitArchivePath).Hash.ToLowerInvariant()
uv --directory services/evaluation run --offline --frozen tradingbot-evaluation research-costs --rates $ratesPath $ratesPin --permit-archive $permitArchivePath $permitPin
```

Для расходов из PostgreSQL используется дата выдачи разрешения по часам
базы. Отчёт сохраняет и фактическое время, сообщённое callback, если оно
доступно. Незавершённые задания, пропущенные ответы и попытки, начавшиеся
до появления журнала разрешений, исключают оценку полной суммы и P95 для
такого задания. Полнота относится к переданному набору архивов. Для
расходов за период в набор включают все задания стратегии за этот период.
[Схема выгрузки](contracts/research-permit-archive.schema.json) · [Пример выгрузки](contracts/valid-research-permit-archive.json).

Для проверки копии PostgreSQL и возобновления работы есть отдельное
локальное упражнение. [Порядок восстановления paper-контура](docs/runbooks/paper.md).

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

Для подтверждённых **виртуальных** исполнений включите профиль
`paper-notifications` в [research Compose](deploy/research/compose.yaml).
Relay читает outbox из PostgreSQL Control, публикует события в
`tradingbot.trade.filled.paper.v1`, а consumer использует отдельную группу и
секреты `TRADINGBOT_PAPER_TELEGRAM_BOT_TOKEN` и
`TRADINGBOT_PAPER_TELEGRAM_CHAT_ID`. Уведомления помечены как симуляция.
`HOLD`, неисполненная заявка и резерв без подтверждённого fill сообщение
о сделке не создают.

```powershell
docker compose -f deploy/research/compose.yaml --profile paper-notifications up -d --build
```

Если Docker Hub недоступен, `TRADINGBOT_GO_BUILD_IMAGE` позволяет задать зеркало
того же образа Go. Локальная сборка проверена с
`mirror.gcr.io/library/golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd`.

## Настоящий research анализ

Настроить API credential выбранного TradingAgents provider в `services/research/.env` или окружении. [Пример окружения](services/research/.env.example). Обновить `snapshot_id` и `as_of` в копии [portfolio-1000.json](services/research/examples/portfolio-1000.json) на действительный snapshot. Цена USDC в примере условно $1.

```powershell
uv --directory services/research run --frozen --extra evaluation tradingbot analyze --portfolio examples/portfolio-1000.json --symbols TSLA NVDA --max-calls 64 --output runtime/research-001
```

При необходимости `--config my-models.json` задаёт только `llm_provider`, `quick_think_llm`, `deep_think_llm` и optional tier provider/endpoints, поддержанные upstream. Не размещать ключи в этом JSON. Текущие default models берутся из pinned upstream, доступ и EU processing нужно подтвердить отдельно.

Для локального анализа с подходящим планом ChatGPT выполните
`uv --directory services/research run --offline --frozen tradingagents login chatgpt`,
откройте ссылку из команды и подтвердите доступ TradingAgents в браузере. Для
`analyze --config` укажите `chatgpt` в `llm_provider`,
`quick_think_provider` и `deep_think_provider`. Обе модели должны быть
доступны по плану. В проверенном запуске использовались `gpt-6-luna` и
`gpt-6-sol`. [Документация OpenAI](https://developers.openai.com/siwc/token-sharing-open-source)

Путь: три upstream analysts → bull/bear → trader/risk/PM для каждой бумаги → **один typed portfolio arbitration** → проверенный research batch. Outputs: `intents.json`, `portfolio.json`, `reports.json`, `metadata.json`. Output directories не перезаписываются. Decimal amounts сериализуются строками для точной передачи в Go.

Для анализа на подготовленных входах передайте локальный архив с записями
`bar`, `filing` и `news` по каждой бумаге. Сборщик
`tradingbot.market_capture.capture_market` сохраняет цену акции с временем
наблюдения и отдельно рассчитывает индикаторы по завершённым дневным барам.
Отчётность и события добавляются в архив с источником и временем получения.
Команда сверяет SHA-256, отбирает записи, полученные к `as_of` портфеля,
и передаёт их напрямую модели принятия решения. Хеши архива и среза
сохраняются в `metadata.json`.

Для NVDA, TSLA, AAPL и MSFT локальная команда сбора автоматически получает
раскрытия и финансовые показатели SEC, официальные ленты NVIDIA, Apple и
Microsoft, затем добавляет цены и индикаторы. Для корпоративных событий
Tesla используются раскрытия SEC. Укажите новый каталог для каждого снимка.
При необходимости передайте контактный User-Agent SEC через
`TRADINGBOT_SEC_USER_AGENT` или `-SecUserAgent`.

```powershell
$capturePath = Join-Path $PWD 'services/research/runtime/research-batch-001'
./services/research/scripts/collect-research.ps1 -Output $capturePath
$archivePath = Join-Path $capturePath 'snapshot/data-archive.json'
$portfolioPath = Join-Path $capturePath 'snapshot/portfolio.json'
$outputPath = Join-Path $capturePath 'result'
$archiveSha = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
uv --directory services/research run --offline --frozen tradingbot analyze --portfolio $portfolioPath --symbols NVDA TSLA AAPL MSFT --data-archive $archivePath --data-sha256 $archiveSha --max-calls 1 --output $outputPath
```

`max-calls` — shared thread-safe ограничение provider calls, не гарантированная сумма USD. SDK retries выключены; tool rounds/output tokens ограничены. Usage сохраняется где provider возвращает metadata; USD cost пока null. Structured output failure/unknown evidence/risk mismatch → error, без BUY по умолчанию и без free-text fallback в наш contract.

### AI по расписанию с виртуальным исполнением

В identity-профиле входа задайте `"analysis_mode": "prepared_provider"` и
`"max_llm_calls": 1` в закреплённом скрининге `0.3`. Скринер применяет
`top_k` и `remaining_analysis_slots`, затем один портфельный вызов оценивает
все выбранные бумаги. Нужен архив `local_archive` с рыночными барами,
раскрытиями и новостями по каждой выбранной бумаге, активными привязками
реестра, наблюдаемыми marks и отдельной лентой paper-котировок. Котировка
для исполнения должна появиться после сохранения результата модели.
Окна дальнейшего пересмотра открытой позиции наследуют режим её открытия.
В плане пересмотра для этого режима также задаётся один вызов на задание.

Для профиля `research-provider` задайте
`TRADINGBOT_RESEARCH_PROVIDER_CONFIG_PATH` с настройкой
`{"llm_provider":"chatgpt","deep_think_llm":"gpt-6-sol"}` или используйте
`"llm_provider":"openai"` с выбранной моделью и `OPENAI_API_KEY` в окружении.
Конфиг модели остаётся без ключей. Для ChatGPT подтвердите OAuth в отдельном
томе этого воркера. Если браузер не может открыть локальный обратный адрес
контейнера, вставьте адрес последней страницы в терминал, где запущен вход.

```powershell
docker compose -f deploy/research/compose.yaml --profile research-provider run --rm --no-deps --entrypoint tradingagents provider-research login chatgpt
docker compose -f deploy/research/compose.yaml --profile research-provider --profile paper-entry --profile paper-review-cycles --profile paper-identity up -d --build
```

Укажите те же пины реестра и данных в настройках Control и планов. Планы
содержат конечный набор окон. Данные и котировки публикует владелец плана.
Профиль выделяет подготовленные задания `0.3` для отдельного воркера,
который получает durable permit до запроса модели и сохраняет квитанцию
использования. Go принимает результат `0.4` после сверки с сохранённой
квитанцией и привязкой к снимку. `paper-identity-dispatch` применяет
допущенное решение к виртуальному счёту. [Архитектура](docs/architecture.md)

Офлайн-проверка использует временную PostgreSQL и тестовую модель на
границе провайдера. Для проверки доставки тех же исполнений создайте
пустую схему PostgreSQL `provider_paper_check`, добавьте
`search_path=provider_paper_check` к URL тестовой базы и установите
`TRADINGBOT_TEST_PROVIDER_NOTIFICATIONS=1`. Отдельный тест Jupiter
проверяет таймаут, повтор диспетчера и учёт результата с имитатором площадки.

```powershell
$env:TRADINGBOT_PROVIDER_TEST_DATABASE_URL = $testDatabaseUrl
uv --directory services/research run --offline --frozen pytest -v tests/test_provider_postgres.py
$env:TRADINGBOT_TEST_DATABASE_URL = $testDatabaseUrl
$env:TRADINGBOT_TEST_JUPITER_NOTIFICATIONS = '1'
go -C services/control test -mod=readonly -count=1 -v -run '^TestScheduledPaperJupiterRehearsal$' ./cmd/paper
```

По умолчанию проверка уведомлений поднимает локальную реализацию протокола
Kafka и заглушку Telegram. Для проверки реального брокера задайте
`TRADINGBOT_TEST_KAFKA_BROKERS` с временным loopback-адресом брокера, на
котором ещё нет paper-топика. Для каждого из двух тестов используйте отдельный
брокер или очистите собственный временный брокер между прогонами.

## Границы research и paper

- `execution_mode` в research schema принимает только `research`.
  Для отправки через Jupiter действует отдельный протокол с одобрением.
  Автономный signer и сверка реальных сделок остаются в live-плане.
- Cash/concentration/holding/action checks не заменяют production Go risk kernel и reservations ledger.
- Upstream data имеет day-level cutoff, не verified intraday PIT. Исторические LLM результаты могут содержать model knowledge leakage.
- Forecast/confidence — оценки модели, не calibration или доказанная alpha. Котировок real execution пока нет.
- Native memory/reflection upstream имеет своё fixed-window settlement; это baseline, не наш фактический net position P&L.
- Real paid LLM run ещё не проверен. Offline integration и native-engine probe не заменяют его.
- Готовность live execution и фактическую доходность отслеживаем отдельно по [плану](docs/planning/plan.md), [backlog](docs/planning/todo.md) и [результатам проверок](docs/verification.md).

## Проверки

```powershell
uv --directory services/research run --frozen --extra evaluation pytest
uv --directory services/research run --frozen --extra evaluation ruff check tradingbot tests scripts
uv --directory services/research build --no-sources
uv --directory services/evaluation run --frozen pytest
go -C services/control test -mod=readonly ./...
go -C services/trade-events test -mod=readonly ./...
go -C services/notifications test -mod=readonly ./...
go -C services/portfolio-analytics test -mod=readonly ./...
```

Чтобы включить PostgreSQL-тесты Go, задайте `TRADINGBOT_TEST_DATABASE_URL`
для отдельной тестовой базы. Тесты создают и удаляют в ней собственные схемы.

Upstream suite запускается отдельно из `services/research/vendor/trading-agents`
через `uv run --project ../.. --frozen --extra evaluation pytest -c pyproject.toml tests`.
[Результаты проверок](docs/verification.md).
