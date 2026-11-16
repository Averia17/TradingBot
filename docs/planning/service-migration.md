# Переход к Go services — 05.10.2026

Дополнение к [исходному backlog](todo.md); не объявляет P0–P8 завершёнными.
[ADR-001](../decisions/001-go-services-python-adapters.md) принят.
Существующие Python CLI и upstream updater продолжают работать.

## [x] S1 — Go control foundation и общий wire contract

**Статус на 14.11.2026.** Python и Go используют общие serialization schemas
и fixtures. Локальный Go control проверен реальным HTTP-запросом на свежем
выводе Python demo. Health подтверждает liveness, capabilities сообщают
о research validation. Денежные строки проходят без округления.

**Scope:** Go module `services/control/`, `cmd/control`, строгая проверка research batch,
health/capabilities и общие Python/Go fixtures. Сохраняем Python CLI и его
входные форматы. На wire передаём decimal strings и явные поля с defaults.

**Acceptance:** реальные Python-emitted batches проходят Go parser; float money,
unknown fields/version/mode, invalid precision/ranges и expiry отклоняются.
Сервис сообщает только реализованные capabilities. Нет queue/ledger ACK без storage.

**Verification:** Python schema/fixture tests, Go parser tests, `vet`, `test`,
`build`, HTTP body/timeouts/cancellation tests. Без LLM calls или кошелька.

## [x] S2 — Durable job/result путь через PostgreSQL

**Статус на 15.11.2026.** Полный цикл проверен на PostgreSQL 17. Через HTTP
подтверждены create/read, claim/heartbeat, повторная выдача после expiry,
публикация артефактов Python demo и чтение после перезапуска control.
Для результата сейчас поддерживается `synthetic_fixture` с нулём LLM calls.
Результаты от Python worker и наблюдаемые usage/provenance добавляются в S3.

**Depends:** S1. Реализуем последовательно:

- [x] **S2a · Durable create/read.** Зафиксировать versioned job envelope и
  ограничения входа. Проверять immutable research snapshot, запрашиваемый universe,
  freshness, policy version и call budget. В одной транзакции записывать snapshot,
  job и событие outbox. Хеш привязывать к определённому wire-представлению, которое
  можно восстановить после чтения из SQL. Повтор ID с тем же содержимым возвращает
  существующую job даже после истечения freshness. Другое содержимое с тем же ID
  job или snapshot возвращает conflict. Ответ queued отправляется после commit.
  Snapshot обозначает исходные данные research.
- [x] **S2b · Attempts.** PostgreSQL атомарно выдаёт job и сохраняет номер
  попытки, claim ID и token. Lease до 60 секунд, deadline попытки раньше из
  15 минут и срока свежести снимка. Heartbeat не продлевает чужую, истёкшую или
  заменённую попытку. После трёх истёкших попыток job получает `failed`.
  Устаревший снимок при обходе очереди получает `expired`.
- [x] **S2c · Completion.** Control проверяет batch по сохранённым portfolio,
  universe и snapshot hash, пересчитывает хеши evidence по тексту отчётов.
  Результат записывается, пока действуют и попытка, и batch. Result, статус
  `completed` и outbox сохраняются одной транзакцией. Совпадающий повтор той же
  попытки возвращает сохранённый результат даже после expiry. Смена попытки
  или содержимого даёт conflict.

**Acceptance:** создать job → leased attempt → записать результат → durable ACK;
restart сохраняет job, duplicate result дедуплицируется, same ID/different hash
отклоняется, expired lease не может записать результат, SQL transaction закрыта
до выполнения внешней работы. Эта очередь ещё не является live order execution.

**Verification:** disposable PostgreSQL. S2a проверяет migrations, restart/read,
rollback без частичных строк, concurrent create и отказ storage без ACK.
В S2b–S2c проверены concurrent claim/complete, сохранение очереди и lease
при миграциях, валидация хешей snapshot/evidence/result, rollback и fencing
до и после expiry. Race gate в CI использует отдельную тестовую PostgreSQL.
P5-01 добавляет расписание и thesis triggers поверх этой очереди.

## [x] S3 — Python research worker поверх существующего adapter

**Depends:** S2. **Scope:** thin service/worker entry point без переписывания graph;
job consume/heartbeat/result protocol, call cap, usage/provenance и cancellation.

- [x] **S3a · Worker loop.** Go fixture worker использует действующий HTTP-контракт.
  Он строит синтетический результат по выданному снимку. Задачи и проверки
  описаны ниже.
- [x] **S3b · TradingAgents graph.** `tradingbot worker` выполняет настоящий граф
  через offline scripted models и публикует `0.2` с usage и provenance.
  Ошибка модели, схемы или лимита останавливает попытку без результата. Worker
  журналирует причину и идентификатор попытки, Control учитывает истечение lease.
  [Сквозная проверка](../verification.md#s3b--python-tradingagents-worker).
- [x] **S3c · Retry budget.** Каждому вызову предшествует durable permit в
  PostgreSQL. При потере ACK или worker слот остаётся списанным, а следующая
  попытка получает только остаток. Количество разрешений сверяется при сохранении
  результата. [Проверка сбоя](../verification.md#s3c--общий-бюджет-вызовов).

**Acceptance:** Go snapshot → real upstream graph с offline models → research
batch → durable Go result. Python не может писать balances/reservations;
provider failure/invalid model output не даёт успешный result. Worker проверяет
lease перед началом и останавливает новые вызовы при потере lease. Общий budget
учитывает retries и попытки с неизвестным usage после сбоя. Протокол результата
связывает usage и provenance с job/attempt, policy, data manifest, origin,
версиями worker/upstream и model config. Неизвестные расходы сохраняются явно.
Credential/API configuration остаётся в worker.

**Verification:** cross-process integration без сети/платных моделей, failure,
lease expiry, duplicate and cancellation fixtures; existing M1 suite зелёная.

### Задачи S3a

**Статус на 16.11.2026.** Команда `research-fixture-worker` проходит полный
цикл HTTP и PostgreSQL с синтетическим результатом. S3a реализован на Go
по запросу владельца. Python остаётся средой для TradingAgents в S3b.
[Результаты проверки](../verification.md#s3a--go-fixture-worker).

1. **HTTP-клиент Go.** Поддержать `claim`, `heartbeat` и `complete` по
   схемам и fixtures из `contracts/`. Задать таймауты. При потере ответа на
   `claim` повторять тот же `claim_id`. При потере ответа на `complete`
   повторять тот же payload с тем же `lease_token`. Проверить `204` при пустой
   очереди, `409` при конфликте и повторы после таймаута.
2. **Запускаемый worker.** Использовать полученные в задании `portfolio` и
   `requested_symbols` для синтетического результата с нулём LLM-вызовов.
   Продлевать lease во время обработки, прекращать попытку при потере lease
   и поддержать остановку процесса. Проверить, что batch связан с полученным
   снимком и что worker прекращает публикацию при известной потере lease.
3. **Сквозная проверка.** Запустить Go control и Go fixture worker отдельными
   процессами с тестовой PostgreSQL. Проверить `create → claim → heartbeat →
   complete → read`, чтение результата после перезапуска Go, повтор completion
   после потери ответа и отклонение результата просроченной попытки. Для
   проверки повторов результат должен сохраниться ровно один раз.

## [x] S4 — Независимая упаковка и проверка обновления

**Depends:** S3. **Scope:** research/evaluation dependency manifests и locks,
immutable images, Compose application profile, release manifest и contract CI.
Никаких signer credentials в application profile.

**Acceptance:** candidate TA worker проходит contract tests с текущим Go;
заменяется только research artifact, control/evaluation версии не меняются;
drain/rollback сохраняет jobs/results/ledger. Evaluation использует Nautilus
в своём окружении. Readiness сообщает фактические version/SHA/protocol.

**Verification:** Compose smoke, artifact digest/provenance, restart/rollback
и compatible/incompatible candidate scenarios. Проверка hosted CI отдельно от YAML.

Локальный [Compose smoke](../../deploy/research/smoke.py) проверил отдельные образы,
метаданные runtime, отклонение неподдерживаемого формата, обработку задач после
замены и отката worker и сохранение результатов. Контрольный кандидат использует
отдельный ID образа с тем же содержимым worker. Этот тест проверяет порядок
замены. Проверка новой версии TradingAgents использует собственную цепочку
совместимости. Hosted CI для этих изменений ещё не наблюдался.

## [ ] S5 — Go ledger/risk/reservations + cost-aware paper execution

**Depends:** S2–S4 и соответствующие PIT/asset registry задачи исходного backlog.
**Scope:** authoritative cash/positions, policy, quote/fill simulation adapter,
reconciliation, costs, Nautilus consumer и honest benchmark.

Отдельный Compose-проект уведомлений включает HTTP-имитацию биржи и Telegram
consumer. Тестовый writer сохраняет fill и задачу pgqueue одной транзакцией.
В S5 нужно подключить публикацию к авторитетной транзакции paper ledger,
сохранив стабильный ID исполнения и отказ от публикации при rollback.
[ADR-002](../decisions/002-trade-notifications.md).

**Acceptance:** отдельный paper account; отсутствие overcommit/double spend,
UNKNOWN удерживает reservation, duplicate/stale отклоняются. Paper intent
однозначно связывает account, held instrument, registry version, raw units и
quote TTL. Ticker из research DTO обозначает underlying. Evaluation учитывает
затраты режима и происхождение данных. Live использует собственный протокол.

**Verification:** ledger invariants/precision, concurrent account decisions,
restart/reconcile, cost accounting и chronological PIT replay tests.

Реализованы автономно проверяемые части S5. Go Control ведёт отдельные виртуальные
счета с точными суммами, версиями, резервами и журналом. `paper-replay` проверяет
хеш тестовой котировки и задаёт детерминированный исход. `unknown` держит резерв,
`cancelled` его освобождает, полное `filled` меняет cash и позицию один раз.
Отдельный Nautilus runtime выполняет хронологический replay тестовых котировок.
Поддержаны денежные выплаты и сопоставимые cash, buy-and-hold и equal-weight
benchmark на синтетическом входе. Go журнал позволяет восстановить FIFO-исходы
и открытые лоты на заданный `as_of`.
Планировщик создаёт идемпотентные задания из готовых снимков и расписания.
Python worker сверяет хеш локального PIT-архива и передаёт доступные ревизии в граф.
Нужны связка модели с paper-исполнением, политика концентрации, частичные
исполнения, сплиты и token multipliers, событийные триггеры и проверяемые данные
для сравнения стратегий. Реальная площадка исключена из текущей работы.

## [ ] S6 — Отдельный executor и independent signer перед live

**Depends:** S5, подтверждённый venue access и исходные security/live gates.
**Scope:** separate credentials/process, decoded transaction policy, bounded
float, signer host/identity и independent chain reconciliation.

**Acceptance:** LLM/control/executor не имеют ключа/Sign/admin; подпись связана
с exact approved payload и owner limits; unsafe/unknown instructions и replay
отклоняются. Настоящие transfers выполняются только в отдельно разрешённой фазе.

**Verification:** integration и security acceptance исходного плана; simulation
перед ограниченным pilot. Этот документ не включает live и не пополняет float.
