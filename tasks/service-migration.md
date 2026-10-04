# Переход к Go services — 05.10.2026

Дополнение к [исходному backlog](todo.md); не объявляет P0–P8 завершёнными.
[ADR-001](../docs/decisions/001-go-services-python-adapters.md) принят.
В этой вехе созданы правила и целевая архитектура. Ни один runtime ticket ниже
пока не выполнен. Существующие Python CLI/upstream updater продолжают работать.

## [ ] S1 — Go control foundation и общий wire contract

**Scope:** Go module `go/`, `cmd/control`, strict research-result boundary,
health/capabilities и общие Python/Go fixtures. Сохранить Python public behavior.
Экспорт schema перевести в serialization mode, обеспечить decimal-string transport.

**Acceptance:** реальные Python-emitted batches проходят Go parser; float money,
unknown fields/version/mode, invalid precision/ranges и expiry отклоняются.
Сервис сообщает только реализованные capabilities. Нет queue/ledger ACK без storage.

**Verification:** Python schema/fixture tests, Go parser tests, `vet`, `test`,
`build`, HTTP body/timeouts/cancellation tests. Без LLM calls или кошелька.

## [ ] S2 — Durable job/result путь через PostgreSQL

**Depends:** S1. **Scope:** migrations, control job API, lease/heartbeat,
immutable result запись и outbox. Единственный account writer.

**Acceptance:** создать job → leased attempt → записать результат → durable ACK;
restart сохраняет job, duplicate result дедуплицируется, same ID/different hash
отклоняется, expired lease не может записать результат, SQL transaction закрыта
до выполнения внешней работы. Эта очередь ещё не является live order execution.

**Verification:** настоящий test PostgreSQL, crash/restart/lease/concurrency tests,
Go race gate. Migrations forward/backward strategy проверена на test данных.

## [ ] S3 — Python research worker поверх существующего adapter

**Depends:** S2. **Scope:** thin service/worker entry point без переписывания graph;
job consume/heartbeat/result protocol, call cap, usage/provenance и cancellation.

**Acceptance:** Go snapshot → real upstream graph с offline models → research
batch → durable Go result. Python не может писать balances/reservations;
provider failure/invalid model output не даёт успешный result. Retry попыток
имеет budget и idempotency; credential/API configuration остаётся в worker.

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

**Acceptance:** отсутствие overcommit/double spend, UNKNOWN удерживает reservation,
дубликаты и stale snapshots отклоняются; native evaluation содержит фактическую
стоимость режима и отдельный synthetic/real data provenance. Research DTO не
получает live permission. Paper success не означает measured live alpha.

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
