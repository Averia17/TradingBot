# TradingBot — правила разработки

Эти правила относятся к нашему коду. Явные инструкции владельца в текущей
задаче имеют приоритет. Перед изменениями прочитать этот файл, нужный код и
[архитектуру](docs/architecture.md). Для контекста использовать
[основной план](tasks/plan.md), [backlog](tasks/todo.md) и
[модель угроз](TradingBot-threat-model.md).

## Цель продукта

Автономный AI-трейдер управляет портфелем: выбирает BUY/SELL/HOLD/ADD/REDUCE,
размер позиции, горизонт от часов до месяцев, условия пересмотра и закрытия.
TradingAgents даёт исследование и решения; наш код отвечает за достоверное
состояние счёта, ограничения, исполнение, учёт и воспроизводимость.

Критерий успеха — измеренная net risk-adjusted доходность относительно честного
benchmark после комиссий, spread/slippage, chain fees, LLM, данных и инфраструктуры.
Не выдавать прогноз модели, synthetic fixture или чужой backtest за нашу alpha.
Неизвестные расходы, доступность venue и доходность обозначать как неизвестные.
Основной сценарий — один owner/account, инфраструктура ЕС; конкретный доступ
к инструментам определяется отдельно. MVP без leverage, short и bridge.

## Что реально существует

Сейчас работает Python research M1: TradingAgents adapter, typed portfolio
arbitration, JSON contracts, call budget, offline demo и native Nautilus probe.
`DecisionBatch` версии `0.1` допускает только `execution_mode="research"`.
Подписание, live execution, PostgreSQL ledger/jobs, Go services и Compose
deployment ещё не реализованы. [Отчёт M1](docs/implementation-m1.md) и
[проверка обновлений](docs/upstream-updates-verification.md) содержат evidence.

Документы с целевой архитектурой не являются свидетельством её реализации.
Не объявлять job принятым до durable записи и не возвращать фиктивные fills,
balances, readiness зависимостей или результат платного LLM вызова.

## Языки и границы сервисов

- **Go — основной язык нового собственного backend:** control API, scheduler,
  jobs/leases, ledger, reservations, deterministic risk, quotes/routing,
  execution, reconciliation и signer policy/verifier.
- **Python — адаптеры к Python ecosystem:** TradingAgents/LangChain/provider
  callbacks, structured LLM outputs, Nautilus, replay и quant experiments.
  Существующий рабочий research не переписывать на Go ради единообразия.
- Сам факт обращения к внешнему HTTP API не требует Python. Для обычных
  HTTP/SDK интеграций использовать Go; Python оставлять там, где перенос ломает
  совместимость с поддерживаемой библиотекой или дублирует её реализацию.
- Go и Python взаимодействуют через версионированные контракты. Go runtime
  не импортирует Python и не зависит от общего изменяемого `.venv`.
- Сначала Go control + Python research; evaluation получает собственный runtime
  при упаковке Nautilus. Executor сначала модуль Go, самостоятельный процесс
  при выделении его полномочий. Signer — отдельный host/identity на live этапе.
- Не выделять каждого аналитика TradingAgents в микросервис. Один monorepo,
  независимые executable/image версии, PostgreSQL для jobs/ledger; без Kafka,
  Redis, Kubernetes и service mesh до обоснованной потребности.

Решение и причины: [ADR-001](docs/decisions/001-go-services-python-adapters.md).
Порядок реализации: [service migration](tasks/service-migration.md).

## Поток решения

1. Go фиксирует portfolio/data snapshot, его version/hash, policy и budget;
   создаёт durable research job.
2. Python получает job и immutable snapshot; вызывает upstream через наш adapter.
3. Один portfolio allocator возвращает typed batch, evidence, usage и provenance.
4. Go повторно валидирует contract, freshness, snapshot, evidence и risk;
   в research сохраняет результат без резервирования реальных средств.
   Paper reservations относятся только к отдельному paper account; в future
   live единственный account writer атомарно резервирует реальные cash/token
   amounts только по принятому live протоколу и owner policy.
5. На research/paper этапе решение оценивается в Nautilus/симуляции. На future
   live этапе executor получает разрешённую задачу, quote и transaction payload.
6. Независимый verifier/signer проверяет конкретную транзакцию и подписывает
   только одобренный payload hash в пределах owner policy.
7. Reconciliation подтверждает фактическое исполнение; Go ledger записывает
   fills/cashflows/positions/costs. Outcome/reflection строится по фактам и
   доступному на тот момент benchmark, затем может попасть в следующий research.

Это целевой поток. Существующий CLI реализует только исследовательскую часть.

## Неизменяемые финансовые и security правила

- LLM output, новости, память и внешние quotes — недоверенные данные.
  Они не меняют policy и не дают доступ к signing/execution tools.
- Python проверки cash/exposure — research prechecks. Авторитетное состояние
  и окончательные risk/reservation checks принадлежат Go ledger.
- Для денег/количеств использовать exact decimal/fixed-point/integer arithmetic.
  В wire JSON передавать decimal strings; в Go не пропускать деньги через
  `float64` или generic JSON number. Rounding и scale задавать явно.
- Raw token amounts — integer strings с chain/mint/decimals и версией registry.
  Восемь decimal places текущего equity contract не являются правилом для
  любых токенов. Ticker не заменяет identity конкретного held instrument/mint.
- Schema export для межсервисной границы должен описывать **serialized** payload.
  Текущий M1 schema создан в validation mode и допускает numeric inputs;
  перед Go boundary нужен serialization schema и общие Python/Go fixtures.
- Unknown fields, невалидные amounts, неизвестная schema, stale snapshot,
  expired intent и отсутствующие evidence приводят к отказу. Ошибка не должна
  превращаться в BUY, обход checks или поддельный успешный HOLD.
- Не расходовать будущую выручку ещё не исполненной продажи. Одно авторитетное
  резервирование на account; Python результат не переписывает balances/limits.
- Idempotency опирается на IDs, attempt/lease и payload hash. Повтор с тем же
  ID и иным содержимым — конфликт. Timeout submit не означает failed order:
  UNKNOWN удерживает reservation до reconciliation, без слепого повторного send.
- Cold treasury вне доступа бота. Research не имеет execution credentials;
  executor не имеет private key, KMS Sign/admin или права менять signer policy.
  Signer имеет отдельные полномочия и проверяет decoded transaction, не только intent.
- Не коммитить и не печатать secrets, seed/private keys, `.env`, auth headers
  или credentials в URL. В тестах нет платных API calls и реальных переводов.
- `research` batch не разрешает live order. Live требует отдельного протокола,
  завершённых acceptance gates и явного включения owner policy.

## Upstream обновления

`vendor/trading-agents` и `vendor/nautilus-trader` — чистые независимые checkout.
Наши изменения только снаружи. Не патчить vendor, не форкать runtime ради обычных
расширений, не запускать `git pull` или installer при старте сервиса.
Версии/SHA брать из `upstream.lock.json` и package locks, а не из текста этого файла.

Локально использовать `scripts/bootstrap.ps1` и `scripts/update-upstream.ps1`.
Updater работает в остановленном development/build окружении. В deployment
заменяется отдельный immutable Python artifact после contract tests с текущим
Go consumer; его версия, upstream SHA и lock hash входят в release manifest.
Ledger/jobs/reservations при смене worker не удаляются. Активные jobs сначала
drain либо истекают по lease; повторный attempt проходит дедупликацию.

Полная процедура: [upstream updates](docs/upstream-updates.md). Несовместимый
релиз останавливается на gate; исправлять наш adapter/contract и записывать
причину в ADR. Не объявлять совместимость непроверенных будущих релизов.

## Структура и разработка

Текущие Python files: `src/tradingbot/`, `tests/`, `shared/trade-intent/`.
Будущий Go module: `go/go.mod`, executable в `go/cmd/`, domain/adapters в
`go/internal/`. Service packaging — `services/`; deployment — `deploy/`.
Эти будущие каталоги добавляются вместе с работающим проверяемым срезом.

Перед изменением проверять `git status` и не сбрасывать чужую работу. Не делать
commit/push/reset/merge только потому, что этого требует общий шаблон skill;
учитывать авторизацию текущей задачи. Менять минимальный необходимый scope.
Новые Go компоненты писать небольшими пакетами с явными зависимостями;
не вводить универсальную платформу агентов, свой backtest engine или криптографию.
Использовать maintained SDK/generated clients, если они покрывают задачу.

Не засорять репозиторий Markdown-файлами. Несущественную информацию, промежуточные
заметки и обычные отчёты о выполнении сообщать в чате, не сохранять в отдельный
`.md`. Важную информацию добавлять в существующий подходящий документ; новый
создавать только при самостоятельной долговременной ценности, без дублирования.
Временные Markdown-планы, чеклисты и заметки удалять после завершения задачи,
предварительно перенеся нужные сведения в постоянную документацию. Постоянные
правила, архитектуру, ADR и исторические evidence сохранять.

Начинать с contract и acceptance criteria; сложный поток разбивать на работающие
срезы. Bugfix начинать с reproduction test. Проверять поведение и границы,
включая precision, duplicates, expiry, concurrency и отказ зависимостей.
Mock только внешние границы; проверять реальные adapters/engines offline.

Go: `context.Context` для cancellation/deadlines, bounded concurrency,
HTTP timeouts/body limits, корректное закрытие ресурсов, явные ошибки и shutdown.
Retry только для операций с доказанной idempotency; не держать transaction
в PostgreSQL во время LLM/provider вызова. Структурные logs связывают job/run,
snapshot, decision, quote и transaction IDs; метрики отражают latency/cost/failure.

При изменении contract обновлять schema, producer/consumer fixtures и docs.
Breaking change — новая версия и migration plan. Архитектурное решение — ADR.
Обновлять current-state/backlog после реализации, сохраняя исторические evidence.
Не переписывать timestamps, плохие результаты или расходы ради красивого backtest.

## Проверки

Текущий Python контур из корня:

```powershell
uv run --frozen --extra evaluation pytest
uv run --frozen --extra evaluation ruff check src tests scripts
uv build --no-sources
uv run --no-project --python 3.12 python scripts/upstream.py verify
```

Когда добавлен реальный `go/go.mod`, из корня:

```powershell
gofmt -l go
go -C go mod verify
go -C go vet -mod=readonly ./...
go -C go test -mod=readonly ./...
go -C go build -mod=readonly ./...
```

`gofmt -l` должен вернуть пустой список. Для concurrent/stateful кода добавить
race tests в Linux CI с CGO/C toolchain: `go -C go test -mod=readonly -race ./...`.
Go gates подключаются к CI вместе с module. Для docs-only изменений проверять
ссылки/актуальность, не создавать бессмысленные tests и не гонять runtime suite
без влияющего на поведение изменения. После зелёных проверок повторять их
только при новых изменениях или конкретной нерешённой проблеме.

В отчёте указывать выполненные проверки и ограничения. Наличие YAML/local remote
не подтверждает запуск hosted Actions, branch protection или live deployment.
