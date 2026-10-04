# Проверки совместимости upstream

`quality.yml` проверяет каждый push и pull request: воспроизводит чистые checkout из `upstream.lock.json`, устанавливает `uv.lock`, проверяет зависимости, lint, наши тесты и сборку на Linux и Windows. На Linux дополнительно запускается стандартный изолированный набор тестов TradingAgents. Его конфигурация исключает тесты внешних сервисов с маркером `integration`; наши тесты запрещают сетевые соединения.

`upstream-compatibility.yml` ежедневно и по ручному запуску проверяет последний стабильный релиз TradingAgents в отдельной среде GitHub Actions. Общий discovery job один раз определяет tag и commit, затем обе системы получают эту пару; updater проверяет ожидаемый commit до запуска тестов. Успешная проверка сохраняет предложенные `upstream.lock.json`, `uv.lock`, `pyproject.toml` и журнал обновления в артефакт. Workflow не изменяет основной репозиторий и не развёртывает бота. Новые версии подключаются локальной командой обновления из основной инструкции проекта.

Ни один workflow не получает ключи LLM, бирж или кошелька. Доступ к репозиторию ограничен `contents: read`; Git-credentials после checkout не сохраняются. Проверка исходных тестов TradingAgents запускается только на Linux: текущий upstream предполагает, что временный каталог находится вне домашнего каталога, что не выполняется на Windows. Контракты и адаптер проверяются на обеих системах без изменения upstream.

Local GitHub remote уже настроен на 05.10.2026; публикация/выполнение hosted Actions и server branch protection здесь не проверены. Scheduled workflow должен находиться на default branch с включёнными Actions. Правила защиты веток задаются в GitHub отдельно: оба статуса `Quality` должны быть обязательными перед merge. Наличие YAML не подтверждает, что эти правила включены. Проект пока не настраивает статический type checker или отдельный vulnerability scanner; `uv pip check` проверяет согласованность зависимостей и не заменяет аудит уязвимостей.

Новый Go CI добавляется вместе с реальным `go/go.mod`, согласно [AGENTS.md](../AGENTS.md) и [service migration](../tasks/service-migration.md). Существующие workflows сейчас проверяют только реализованный Python контур.

Внешние Actions зафиксированы по commit. Источники проверены 2026-10-04:

- [actions/checkout v7.0.1](https://github.com/actions/checkout/releases/tag/v7.0.1)
- [astral-sh/setup-uv v10.2.0](https://github.com/astral-sh/setup-uv/releases/tag/v10.2.0)
- [actions/upload-artifact v7.0.1](https://github.com/actions/upload-artifact/releases/tag/v7.0.1)

Dependabot раз в неделю предлагает обновления самих Actions. Такие предложения также проходят обычный `Quality`.
