# TradingBot — правила разработки

Эти правила относятся к нашему коду. Явные инструкции владельца в текущей
задаче имеют приоритет. Перед изменениями прочитать этот файл, нужный код и
[архитектуру](docs/architecture.md). Для контекста использовать
[основной план](docs/planning/plan.md), [backlog](docs/planning/todo.md) и
[модель угроз](docs/threat-model.md).

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
Trading API проверяет research batch и с PostgreSQL ведёт задания, аренды,
общий бюджет вызовов и результаты. Python worker исполняет pinned граф
TradingAgents с офлайн-моделями и архивными снимками. Go ведёт виртуальный
счёт, точные резервы, полные и частичные исполнения. `paper` связывает
результат research и виртуальное исполнение по тестовой котировке с policy
владельца. Nautilus проверяет исторические сценарии в отдельном окружении.
Go планировщик пересмотра создаёт durable research jobs из состояния
paper-позиции и хешированных архивных событий. Trading API, Accounts и Trade
Events владеют отдельными PostgreSQL. Процессы orders и relay получают чужие
данные по HTTP. События исполнений через outbox Accounts попадают в pgqueue
Trade Events и Kafka. [ADR-005](docs/decisions/005-service-databases.md). Отдельный Compose-проект
содержит тестовый HTTP-вход и Telegram consumer.
Подписание, live execution и развёртывание live-контура остаются в будущих
срезах.
[Результаты M1 и upstream-проверок](docs/verification.md) содержат evidence.

Документы с целевой архитектурой не являются свидетельством её реализации.
Не объявлять job принятым до durable записи и не возвращать фиктивные fills,
balances, readiness зависимостей или результат платного LLM вызова.

## Языки и границы сервисов

- **Go — основной язык нового собственного backend:** Trading API, scheduler,
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
- Сначала Trading API + Python research; evaluation получает собственный runtime
  при упаковке Nautilus. Executor сначала модуль Go, самостоятельный процесс
  при выделении его полномочий. Signer — отдельный host/identity на live этапе.
- Не выделять каждого аналитика TradingAgents в микросервис. Один monorepo,
  независимые executable/image версии, PostgreSQL для jobs/ledger. Kafka используется
  для уведомлений о fills по [ADR-002](docs/decisions/002-trade-notifications.md).
  Redis, Kubernetes и service mesh добавлять только при обоснованной потребности.

Решение и причины: [ADR-001](docs/decisions/001-go-services-python-adapters.md).
Порядок реализации: [service migration](docs/planning/service-migration.md).

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

В локальном контуре исследование и виртуальное исполнение используют
раздельные команды и явную политику владельца.

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
- Schema export для межсервисной границы описывает **serialized** payload.
  Поддерживать совместимые JSON-схемы и fixtures в `contracts/`.
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

`services/research/vendor/` хранит чистые независимые checkout TradingAgents и Nautilus.
Наши изменения только снаружи. Не патчить vendor, не форкать runtime ради обычных
расширений, не запускать `git pull` или installer при старте сервиса.
Версии/SHA брать из `services/research/upstream.lock.json` и package locks.

Локально использовать `services/research/scripts/bootstrap.ps1` и
`services/research/scripts/update-upstream.ps1`.
Updater работает в остановленном development/build окружении. В deployment
заменяется отдельный immutable Python artifact после contract tests с текущим
Go consumer; его версия, upstream SHA и lock hash входят в release manifest.
Ledger/jobs/reservations при смене worker не удаляются. Активные jobs сначала
drain либо истекают по lease; повторный attempt проходит дедупликацию.

Полная процедура: [upstream updates](docs/upstream-updates.md). Несовместимый
релиз останавливается на gate; исправлять наш adapter/contract и записывать
причину в ADR. Не объявлять совместимость непроверенных будущих релизов.

## Структура и разработка

- `services/research/` содержит Python-пакет `tradingbot`, тесты, locks и updater.
- `services/trading-api/` содержит Trading API, очередь research jobs и учёт
  виртуального счёта в пакете `internal/accounts`.
- `services/trade-events/` владеет тестовым exchange endpoint и pgqueue outbox.
- `services/notifications/` владеет Kafka consumer и отправкой в Telegram.

Каждый сервис хранит свой код, зависимости и тесты в собственном каталоге.
Граница между сервисами проходит через версионированные форматы обмена.
`contracts/` содержит JSON-схемы и fixtures. Compose находится в `deploy/`.

Для составных имён пакетов и файлов с исходным кодом используем `snake_case`.
В Go имя пакета совпадает с именем каталога. Функции, типы и переменные
в Go именуем в `camelCase` или `PascalCase` в зависимости от экспорта.

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
Экспорт Python обновляет `contracts/`. Trading API проверяет вход по встроенным
JSON-схемам. Их нужно обновлять вместе с опубликованным контрактом.
Breaking change — новая версия и migration plan. Архитектурное решение — ADR.
Обновлять current-state/backlog после реализации, сохраняя исторические evidence.
Не переписывать timestamps, плохие результаты или расходы ради красивого backtest.

## Проверки

Текущий Python контур из корня:

```powershell
uv --directory services/research run --frozen --extra evaluation pytest
uv --directory services/research run --frozen --extra evaluation ruff check tradingbot tests scripts
uv --directory services/research build --no-sources
uv --directory services/research run --no-project --python 3.12 python scripts/upstream.py verify
```

Go-проверки из корня на примере `services/trading-api`.

```powershell
gofmt -l services/trading-api services/trade-events services/notifications
go -C services/trading-api mod verify
go -C services/trading-api vet -mod=readonly ./...
go -C services/trading-api test -mod=readonly ./...
go -C services/trading-api build -mod=readonly ./...
```

Дополнительные Go-модули проверять теми же командами, задав их каталог в `-C`.
`gofmt -l` должен вернуть пустой список. Для concurrent/stateful кода добавить
race tests в Linux CI с CGO и C toolchain через `go test -mod=readonly -race ./...`.
Go gates подключаются к CI вместе с module. Для docs-only изменений проверять
ссылки/актуальность, не создавать бессмысленные tests и не гонять runtime suite
без влияющего на поведение изменения. После зелёных проверок повторять их
только при новых изменениях или конкретной нерешённой проблеме.

В отчёте указывать выполненные проверки и ограничения. Наличие YAML/local remote
не подтверждает запуск hosted Actions, branch protection или live deployment.
