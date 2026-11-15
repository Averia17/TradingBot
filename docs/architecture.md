# Архитектура TradingBot: Go services и Python adapters

**Решение принято 05.10.2026.** Это целевая архитектура и порядок перехода.
Доступны Python research CLI, локальная stateless-валидация и research queue
в Go control с durable create/read, claim, heartbeat и сохранением синтетического
результата. Python worker и deployment входят в следующие срезы.
Правила для разработчиков: [AGENTS.md](../AGENTS.md).

## Ответственность компонентов

| Компонент | Язык | Ответственность | Runtime статус |
|---|---|---|---|
| Control/account | Go | API, scheduler, jobs/leases/outbox, snapshots, ledger, reservations, deterministic risk, costs, reconciliation | `services/control/`: health, capabilities, stateless validation, PostgreSQL jobs, leases и результаты demo |
| Research worker | Python | TradingAgents graph, provider SDK/callbacks, portfolio arbitration, evidence, usage; выдаёт research batch | `services/research/`: CLI и library, worker entry point впереди |
| Evaluation worker | Python | Nautilus replay/backtest/paper evaluation, execution-cost models, benchmarks | Сейчас synthetic native probe |
| Execution worker | Go | Allowed venue quotes/build/submit/status; без ключа и права менять policy | Будущий модуль control, затем отдельный process |
| Independent verifier/signer | Go | Decode transaction, owner limits/allowlists, approval exact hash, signing | Отдельный future live host/identity |
| Trade events | Go | Принимает тестовые fills и публикует события через pgqueue и Kafka | `services/trade-events/`, независимый образ в Compose-профиле `test` |
| Notifications | Go | Читает события из Kafka, отправляет в личный Telegram | `services/notifications/`, независимый образ |
| PostgreSQL | SQL | Durable state/jobs/events и constraints; economic writes только через control | Локальные migrations для research snapshots/jobs/outbox, production deployment впереди |

Go — основной язык собственного backend. Python сохраняем для библиотек, которые
уже предоставляют graph, structured inference и evaluation engine. Обычные внешние
HTTP API доступны из Go: подключение к удалённому сервису само по себе не причина
создавать Python компонент. Языковая граница следует dependency и полномочиям.
Ускорение provider inference этой архитектурой не измерено; производительность
нашего кода проверяем профилированием и latency/cost метриками.

Сначала два service runtime: Go control и Python research. Evaluation запускается
offline по отдельному профилю и получает независимое окружение при упаковке
Nautilus. Executor становится отдельным service перед выделением его production
credentials; signer всегда отдельная signing boundary. Аналитики внутри
TradingAgents остаются частью одного upstream graph.

## Целевой поток

```mermaid
flowchart LR
    DATA[Market data / news / actions] --> SNAP[Immutable data snapshot]
    subgraph APP[EU application host]
        CTRL[Control/account service: Go]
        DB[(PostgreSQL ledger + jobs + outbox)]
        RES[Research worker: Python + TradingAgents]
        EVAL[Evaluation worker: Python + Nautilus]
        EXEC[Execution worker: Go, future]
        CTRL <--> DB
        CTRL -->|job + portfolio snapshot + budget| RES
        RES -->|typed research batch + evidence + usage| CTRL
        CTRL -->|frozen snapshots + decisions| EVAL
        EVAL -->|cost-aware results + benchmark| CTRL
        CTRL -->|validated intent + reservation| EXEC
        EXEC -->|observations for reconciliation| CTRL
    end
    SNAP --> CTRL
    RES <--> LLM[External LLM API]
    EXEC <--> VENUE[Quote/build/submit/status API]
    subgraph SIGN[Separate host and identity: future live]
        VERIFY[Independent transaction verifier]
        KEY[Signer / bounded hot float]
        VERIFY --> KEY
    end
    EXEC -->|exact transaction + policy context| VERIFY
    KEY -->|signature for approved hash| EXEC
    OWNER[Owner cold treasury and policy] --> VERIFY
```

Control выбирает разрешённый universe и immutable state. AI определяет торговое
решение; Go проверяет его допустимость. Go kernel не подменяет стратегию набором
BUY/SELL сигналов. Python batch не является командой подписи и не переписывает
авторитетный portfolio state. Research/paper stages не подключают signer.

Research сохраняет решения без блокировки реальных средств. Paper ledger и
его reservations отделены от live account. Резервирование реальных cash/tokens
появляется только в разрешённом live протоколе и не выводится из research DTO.

## Межсервисный contract

Начинаем с HTTP/JSON и явного protocol version. Длительный LLM/backtest workflow
выполняется как job: bounded request выдаёт durable job ID, worker использует
lease/attempt/heartbeat, результат публикуется отдельно. Открытый HTTP-запрос
на всё время анализа не является механизмом очереди.

Job envelope `0.1` для create/read содержит immutable research portfolio,
ссылку и хеш data manifest, requested symbols, policy version и call budget.
Control возвращает durable ACK после commit snapshot, job и creation outbox.
PostgreSQL выдаёт и продлевает один действующий lease на job по своему времени.
Попытку идентифицируют worker ID, claim ID, номер и уникальный token.
Повтор того же claim в пределах lease возвращает уже выданную попытку.
Сейчас completion принимает batch, отчёты и метаданные `synthetic_fixture`.
Control сравнивает batch с сохранённым snapshot и universe, пересчитывает
evidence hashes из текста и проверяет lease перед записью. Результат, статус
и outbox commit атомарно. S3 добавляет actual usage и provenance моделей и upstream
для Python worker. Существующий research DTO
содержит `run_id`, `portfolio_snapshot_id`, `as_of`, `created_at`, `expires_at`,
`evidence_hashes`, `intents` и `schema_version="0.1"`.

| Инвариант границы | Требование |
|---|---|
| Money/quantity | Decimal strings, exact arithmetic, explicit scale/rounding; raw token units integer strings |
| Schema | Strict fields/types/version; serialized schema и общие producer/consumer fixtures |
| Provenance | Snapshot/version/hash, evidence references, config/upstream/artifact versions |
| Freshness | UTC timestamps, expiry, stale portfolio refusal, quote TTL separately |
| Idempotency | Job/run/result IDs + attempt/lease + payload hash; same ID/different hash — conflict |
| Authority | Result endpoint не принимает новые balances, owner policy, credentials или signing instructions |
| Availability | Durable ACK после transaction commit; UNKNOWN execution удерживает reservation |

Research DTO описан [общими JSON Schema и fixtures](../contracts/).
Python сериализует amounts десятичными строками. Go проверяет схему,
freshness и согласованность распределения с переданным portfolio и universe.
И диагностика, и локальное создание job получают research portfolio от
вызывающей стороны. Сохранённая запись фиксирует этот ввод. Control проверяет
сохранённые хеши перед выдачей и проверяет attempt и snapshot при записи результата.
Доступность содержимого data manifest проверяется при интеграции с архивом.
Право на paper/live execution задаётся отдельными протоколами.

## Данные и полномочия

PostgreSQL — единый authority экономического состояния. Control владеет ledger,
reservations и account serialization. Job creation и outbox фиксируются атомарно;
worker не держит SQL transaction во время provider calls. Research работает
через control contract без прямых write credentials к balances/reservations.
Evaluation может иметь собственную область хранения результатов, с ограниченной
ролью; она не меняет реальный ledger. Research jobs используют PostgreSQL.
Kafka используется для событий об исполненных сделках по [ADR-002](decisions/002-trade-notifications.md).
`trade-events` владеет outbox и соединением с PostgreSQL. `notifications`
имеет доступ к Kafka и Telegram. Сервисы собираются из отдельных Go-модулей,
обмениваются событиями по версионированному JSON-контракту и разворачиваются
через [Compose](../deploy/notifications/compose.yaml).

Большие evidence/data objects хранятся как immutable files/object archive;
в SQL — hashes, timestamps, versions и ссылки. Outputs и memory lessons не
дают исполнительных полномочий. Расходы и outcomes сохраняются воспроизводимо.
Live reconciliation подтверждает реальные fills/finality по независимым
наблюдениям; timeout сам по себе не освобождает cash и не разрешает resend.

Deployment: monorepo, независимые binaries/images, приватная service network,
раздельные credentials/limits и Docker Compose на первом application host.
Signer имеет отдельный host/identity/policy, вне обычного Compose application
trust boundary. Kubernetes/service mesh не требуются текущим scale и SLA.
Эти ограничения будут проверяться acceptance tests, сейчас это проектное решение.

## Независимые обновления

TradingAgents — библиотека внутри нашего research service. Репозиторий остаётся
clean checkout, адаптер находится в `services/research/tradingbot/adapters/tradingagents.py`.
Service artifact фиксирует exact source SHA, Python lock hash и contract version.
Go consumer знает наш protocol; имена upstream Python functions для него закрыты.

Обновление проходит так:

1. В отдельном build окружении получить candidate release по tag и SHA.
2. Обновить только research зависимости; провести upstream adapter, producer
   и consumer compatibility tests против текущего Go artifact.
3. Выпустить immutable research image/artifact с manifest/digest. Readiness
   подтверждает конфигурацию, upstream capability и поддерживаемый protocol.
4. Остановить получение новых jobs старым worker; drain активные attempts или
   дождаться корректного lease expiry. Передать jobs новому worker.
5. Контролировать failures/usage/latency; при regression вернуть прежний artifact,
   сохранив ledger, job IDs, reservations и audit history.

Пока общий dev `uv.lock` обслуживает оба Python компонента. Для настоящей
deployment isolation research и evaluation получат свои dependency manifests,
locks и artifacts; это часть service packaging, а не уже существующая гарантия.
Go/control и execution/signer сохраняют собственные версии при обновлении TA,
если наш protocol не изменился. Breaking protocol change требует versioned
migration и совместного rollout, без ручного merge с чужими исходниками.

Рабочий updater применяется к остановленному dev/build checkout. В запущенном
сервисе не выполняются `git pull`, `uv sync` или замена `.venv`.
[Текущая процедура](upstream-updates.md) и
[план перехода](planning/service-migration.md) задают разные этапы.
