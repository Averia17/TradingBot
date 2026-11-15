# Проверки совместимости upstream

`quality.yml` проверяет каждый push и pull request: воспроизводит чистые checkout из `upstream.lock.json`, устанавливает `uv.lock`, проверяет зависимости, lint, наши тесты и сборку на Linux и Windows. На Linux дополнительно запускается стандартный изолированный набор тестов TradingAgents. Его конфигурация исключает тесты внешних сервисов с маркером `integration`; наши тесты запрещают сетевые соединения.

`upstream-compatibility.yml` ежедневно и по ручному запуску проверяет последний стабильный релиз TradingAgents в отдельной среде GitHub Actions. Общий discovery job один раз определяет tag и commit, затем обе системы получают эту пару; updater проверяет ожидаемый commit до запуска тестов. Успешная проверка сохраняет предложенные `upstream.lock.json`, `uv.lock`, `pyproject.toml` и журнал обновления в артефакт. Workflow не изменяет основной репозиторий и не развёртывает бота. Новые версии подключаются локальной командой обновления из основной инструкции проекта.

Workflows используют `contents: read` и работают без ключей LLM, бирж и кошелька. Исходный набор тестов TradingAgents запускается в Linux job. Наши контракты и адаптер проверяются на обеих системах.

Плановый workflow запускается на default branch репозитория. История проверок доступна в [GitHub Actions](https://github.com/Averia17/TradingBot/actions). Правила обязательных проверок перед merge задаются отдельно в GitHub.

`Go Quality (ubuntu-latest)` в `quality.yml` проверяет форматирование, зависимости, `vet`, тесты, гонки данных и сборку `services/control`, `services/trade-events` и `services/notifications`. Версия Go берётся из `services/control/go.mod`. Race gate использует отдельную PostgreSQL 17 для тестов research jobs и outbox. Тесты Kafka и Telegram используют локальные тестовые реализации. Этапы реализации описаны в [service migration](../docs/planning/service-migration.md).

Внешние Actions зафиксированы по commit. Источники проверены 2026-11-14.

- [actions/checkout v7.0.1](https://github.com/actions/checkout/releases/tag/v7.0.1)
- [actions/setup-go v7.0.0](https://github.com/actions/setup-go/releases/tag/v7.0.0)
- [astral-sh/setup-uv v10.3.3](https://github.com/astral-sh/setup-uv/releases/tag/v10.3.3)
- [actions/upload-artifact v7.0.2](https://github.com/actions/upload-artifact/releases/tag/v7.0.2)

Dependabot раз в неделю предлагает обновления самих Actions. Такие предложения также проходят обычный `Quality`.
