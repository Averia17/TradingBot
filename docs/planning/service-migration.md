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

## [ ] S3 — Python research worker поверх существующего adapter

**Depends:** S2. **Scope:** thin service/worker entry point без переписывания graph;
job consume/heartbeat/result protocol, call cap, usage/provenance и cancellation.

- **S3a · Worker loop.** Python клиент вызывает существующие claim, heartbeat и
  completion. Проверить тот же claim ID после сетевого сбоя, остановку при потере
  lease и запуск Go + Python на PostgreSQL с demo adapter.
- **S3b · TradingAgents graph.** Подключить текущий adapter с offline scripted
  models. Контракт результата передаёт источник, usage, версии и config hash.
  Ошибки provider, schema и budget видны как неуспешная попытка.
- **S3c · Retry budget.** Ограничить общий расход между попытками. Если worker
  завершился без usage, следующий attempt получает только гарантированно
  доступный остаток. Проверить cancellation во время graph и перед публикацией.

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

## [ ] S4 — Независимая упаковка и проверка обновления

**Depends:** S3. **Scope:** research/evaluation dependency manifests и locks,
immutable images, Compose application profile, release manifest и contract CI.
Никаких signer credentials в application profile.

**Acceptance:** candidate TA worker проходит contract tests с текущим Go;
заменяется только research artifact, control/evaluation версии не меняются;
drain/rollback сохраняет jobs/results/ledger. Evaluation использует Nautilus
в своём окружении. Readiness сообщает фактические version/SHA/protocol.

**Verification:** Compose smoke, artifact digest/provenance, restart/rollback
и compatible/incompatible candidate scenarios. Проверка hosted CI отдельно от YAML.

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

## [ ] S6 — Отдельный executor и independent signer перед live

**Depends:** S5, подтверждённый venue access и исходные security/live gates.
**Scope:** separate credentials/process, decoded transaction policy, bounded
float, signer host/identity и independent chain reconciliation.

**Acceptance:** LLM/control/executor не имеют ключа/Sign/admin; подпись связана
с exact approved payload и owner limits; unsafe/unknown instructions и replay
отклоняются. Настоящие transfers выполняются только в отдельно разрешённой фазе.

**Verification:** integration и security acceptance исходного плана; simulation
перед ограниченным pilot. Этот документ не включает live и не пополняет float.
