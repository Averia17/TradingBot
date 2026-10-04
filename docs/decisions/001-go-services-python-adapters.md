# ADR-001: Go services, Python upstream adapters

## Status

Accepted, 2026-10-05. Архитектурное решение принято; service runtime ещё не построен.
Ранее описанные в `tasks/plan.md` Go execution и Python research сохраняются;
это решение уточняет Go как основной язык нового backend и clean upstream policy.

## Context

Владелец хочет писать собственный backend на Go, сохранить Python там, где он
нужен TradingAgents/Nautilus, и обновлять эти компоненты независимо. Уже есть
проверенный Python research M1, clean upstream checkouts и release updater.
Ledger, job protocol, Go kernel и deployment пока отсутствуют. Микросервисная
граница должна защищать state/credentials и не требовать merge с чужим кодом.

## Decision

- Новые control/state/risk/execution сервисы пишем на Go; исследовательский
  graph, provider SDK и Nautilus остаются Python adapters.
- Один monorepo, отдельные runtime artifacts. Первые сервисы — Go control и
  Python research. Evaluation получает независимый Python lock/environment;
  executor сначала модуль Go, отдельный process при выделении credentials.
- Control владеет PostgreSQL ledger/reservations и durable jobs/outbox.
  Python не имеет write authority к экономическому состоянию.
- Versioned HTTP/JSON для bounded commands/results; длительная работа через
  jobs/leases. Exact decimal strings и cross-language fixtures на границе.
- Clean upstream SHA + our adapter + compatibility tests. Новая версия TA
  выпускает research artifact; updater не меняет запущенную `.venv`.
- Signer/verifier — отдельный Go runtime, host и identity перед live.
  Research/control/executor не получают ключ или Sign/admin capability.
- Docker Compose и PostgreSQL достаточны для первого deployment. Сетевые
  границы, timeout/retry/idempotency и observability входят в acceptance.

## Alternatives considered

| Вариант | Причина выбора другого решения |
|---|---|
| Переписать TradingAgents/Nautilus на Go | Дублирование graph/engine и зависимость от ручного сопровождения каждого upstream изменения |
| Всё в одном Python runtime | Не соответствует предпочтению владельца и связывает обновления research с control/execution dependencies |
| Каждый analyst как service | Усложняет upstream graph, latency и ops без текущей потребности |
| Сразу Kafka/Kubernetes/gRPC для каждого шага | Дополнительные operational contracts; текущие jobs и scale не требуют такого комплекса |
| Общая изменяемая `.venv` в deployment | Обновление одного компонента затрагивает работающие процессы и лишает artifact воспроизводимости |

## Consequences

Появляются network/job contracts, consumer/provider fixtures и отдельные release
manifests. Go financial/state logic нужно проверять независимо от Python prechecks.
Производительность собственного кода измеряется; переход не является доказательством
ускорения remote inference или положительной торговой alpha.

Миграция проходит работающими срезами, сохраняя CLI и research-only режим до
следующих gates. Описание и очередность:
[architecture](../architecture.md), [service migration](../../tasks/service-migration.md).
