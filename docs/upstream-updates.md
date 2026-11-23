# Чистые upstream и наши адаптеры

Решение: upstream репозитории клонируются без наших изменений. Наш проект хранит
контракты, политики, адаптеры и тесты отдельно. `services/research/vendor/`
исключён из нашего Git. Публичные URL, release tag и точный commit записаны в
`services/research/upstream.lock.json`.
Bootstrap воспроизводит эти checkout на другой машине. Fork для этого не нужен.

```text
services/research/                  Python research service
  tradingbot/adapters/              интерфейсы внешних компонентов
    tradingagents.py               imports/config/context/graph/report boundary
  tradingbot/contracts.py           наш стабильный JSON contract и проверки
  tradingbot/research.py            общий portfolio arbitration
  tradingbot/nautilus_probe.py      отдельная граница Nautilus engine
  tests/                           offline contract/integration/update tests
  scripts/upstream.py               clone / verify / check / tested update
  upstream.lock.json               проверенные upstream tag + SHA
  uv.lock                          проверенные Python dependencies
  vendor/                          отдельные ignored Git checkout
    trading-agents/                clean detached HEAD, remote upstream
    nautilus-trader/                clean detached HEAD, reference source
```

Python подключает TradingAgents как editable dependency из чистого checkout.
Изменять файлы там нельзя: расширения делаются в нашем пакете. Для Nautilus
checkout служит исходным reference, а runtime устанавливается из официального
wheel с точной версией. Обновление reference без соответствующего runtime не
считается проверенной миграцией.

## Пины и лицензии

Сверка 18.11.2026. Даты в таблице — `published_at` релиза GitHub в UTC. Теги
проверены через точные `refs/tags/...`, а файлы LICENSE прочитаны из Git-объектов
закреплённых коммитов. Локальные HEAD и Git blob LICENSE совпали с upstream.

| Компонент | Релиз и commit | Лицензия и evidence |
|---|---|---|
| TradingAgents, runtime research | [v0.7.1](https://github.com/TauricResearch/TradingAgents/releases/tag/v0.7.1), 27.10.2026 08:43:04 UTC, `c4544913a14ed3df5f1ed7b9e8483270ce20682a` | Apache-2.0, [LICENSE](https://github.com/TauricResearch/TradingAgents/blob/c4544913a14ed3df5f1ed7b9e8483270ce20682a/LICENSE), blob `261eeb9e9f8b2b4b0d119366dda99c6fd7d35c64` |
| NautilusTrader, source reference и wheel BacktestEngine | [v1.231.0](https://github.com/nautechsystems/nautilus_trader/releases/tag/v1.231.0), 02.08.2026 18:53:49 UTC, `27a8e54e7ac3c57d6cbf8891f0283dfbaee97317` | LGPL-3.0-or-later в `pyproject.toml` и metadata wheel, [LICENSE](https://github.com/nautechsystems/nautilus_trader/blob/27a8e54e7ac3c57d6cbf8891f0283dfbaee97317/LICENSE), blob `5550e2db15f239ea8d3cf54bfa3b035eab8d3174` |

[Upstream lock](../services/research/upstream.lock.json) хранит оба commit,
лицензии и пустые списки `patches`. Исторический тестовый патч v0.6.0 хранится
отдельно и исключён из bootstrap. В Dockerfile research копируются наш пакет
и manifests, а TradingAgents получается из закреплённого checkout. Dockerfile
evaluation копирует наш evaluator и устанавливает закреплённый wheel
NautilusTrader. В списках зависимостей, в поставляемом коде и в путях сборки
ссылок на ATLAS или его закрытые промпты не найдено.

[Research lock](../services/research/uv.lock) фиксирует версии, даты загрузки
и SHA-256 wheel/sdist для LangChain, LangGraph и SDK провайдеров. По `LICENSE`
установленных пакетов сверены `langchain-core 1.6.6`, `langgraph 1.2.12`,
`langgraph-sdk 0.4.5`, `langchain-openai 1.6.7`, `langchain-anthropic 1.7.5`,
`langchain-google-genai 4.4.0` и `anthropic 1.11.0` (MIT),
`openai 3.24.0` и `google-genai 2.28.0` (Apache-2.0 на сам пакет).
[Evaluation lock](../services/evaluation/uv.lock) отдельно фиксирует wheel
NautilusTrader по платформам и `msgspec 0.22.0` (BSD-3-Clause). Go-зависимости
закреплены в `go.mod` и `go.sum` каждого сервиса. В модуле
`pgkit/v2` на `d8cfea48af9d2031f602d7c09ecc255c4542d439` условия Apache 2.0
заявлены в [README исходника](https://github.com/goware/pgkit/blob/d8cfea48af9d2031f602d7c09ecc255c4542d439/README.md#license).
Отдельный текст LICENSE в этом коммите и модуле отсутствует.

При передаче сборок третьим лицам Apache-2.0 §4 предусматривает сохранение
лицензии и применимых уведомлений об авторстве, включая upstream NOTICE при
его наличии, а MIT и BSD требуют сохранять соответствующие тексты и уведомления.
Для Nautilus LGPLv3 §4 описывает уведомления, тексты GPL/LGPL и возможность
использовать изменённую совместимую библиотеку или выполнить требования по
передаче соответствующих исходников. Публичный комплект таких материалов для
контейнеров и полный перечень их транзитивных лицензий пока не проверены.
Совместимость подтверждена для закреплённых версий. Доступ к платным моделям
и миграция Nautilus на v2 остаются открытыми.

Из корня проекта локальные пины проверяются командами
`uv --directory services/research run --offline --no-project --python 3.12 python scripts/upstream.py verify`,
`uv --directory services/research lock --offline --check` и
`uv --directory services/evaluation lock --offline --check`.
Go-модули проверяются `go mod verify` в каталогах
`services/trading-api`, `services/trade-events` и `services/notifications`.

## Обновление

В PowerShell перейдите в `services/research` из корня проекта.

```powershell
Set-Location services/research

# Воспроизвести проверенные исходники и окружение
.\scripts\bootstrap.ps1

# Узнать последний стабильный release без смены версии
.\scripts\update-upstream.ps1 -CheckOnly

# Обновить TradingAgents до последнего стабильного release и проверить
.\scripts\update-upstream.ps1

# Явный release, в том числе для проверенного возврата назад
.\scripts\update-upstream.ps1 -Release v0.6.0

# Nautilus обновляется отдельно, с повторной проверкой native engine
.\scripts\update-upstream.ps1 -Repository nautilus-trader
```

Кроссплатформенные эквиваленты также запускаются из `services/research`.

```text
uv run --no-project --python 3.12 python scripts/upstream.py bootstrap
uv sync --frozen --extra evaluation
uv run --no-project --python 3.12 python scripts/upstream.py verify
uv run --no-project --python 3.12 python scripts/upstream.py check --repo trading-agents
uv run --no-project --python 3.12 python scripts/upstream.py update --repo trading-agents
```

Updater предназначен для рабочего checkout разработки: выполнять его нужно
при остановленных процессах, использующих его `.venv`. Он не участвует в запуске
анализа, не меняет работающий live deployment и не обращается к LLM или кошельку.
Для future production переносится проверенный набор SHA/lock в отдельное
окружение; deployment/restart остаётся отдельной операцией.

Транзакция обновления:

1. Проверить независимые Git checkout, их remote URL, отсутствие локальных
   изменений/неотслеживаемых файлов и соответствие HEAD нашему lock.
2. Получить latest stable release через официальный GitHub API или выбрать
   указанный version tag. `main`, draft и prerelease не принимаются.
3. Fetch конкретного тега без перезаписи локальных тегов, разрешить annotated
   tag до commit SHA. Смена SHA уже проверенного release отклоняется.
4. Сохранить исходные HEAD и байты `pyproject.toml`, `uv.lock`,
   `upstream.lock.json`; переключить checkout на candidate в detached HEAD.
5. Обновить точный package requirement, разрешить зависимости через uv,
   установить frozen lock и выполнить dependency metadata check, doctor,
   Ruff, весь наш pytest suite и build.
6. При успехе записать новый tag/SHA в upstream lock и отчёт в
   `runtime/upstream-updates/`. При ошибке вернуть HEAD, файлы и установить
   прежнее frozen окружение. Ошибка самого восстановления явно сообщается.

Параллельные bootstrap/update отклоняются через `.upstream-update.lock`.
После аварийного завершения процесса этот файл может остаться: сначала нужно
проверить PID, текущие HEAD и lock-файлы, восстановить согласованное состояние,
затем удалить только файл блокировки. Восстановление после выключения питания
не гарантируется; Git сохраняет исходный commit, но backups транзакции в памяти.
Dirty checkout не сбрасывается и не очищается автоматически.
Если первый fetch прервался, bootstrap может продолжить пустой Git checkout
без HEAD с проверенным remote. Наличие любых пользовательских файлов, включая
ignored файлы, запрещает это возобновление; они не удаляются.

## Совместимость новых релизов

Адаптер проверяет реальные доступные exports, signature графа с `portfolio`,
модель portfolio context, конфигурацию и форму результата. Номер версии сам по
себе не запрещает обновление. Тест запускает настоящий upstream graph с
scripted LangChain models и запрещённой сетью: проверяется передача свободного
cash, реальных quantities и получение analyst/PM reports. Наши строгие JSON,
evidence и risk checks проверяются независимо.

Это устраняет merge нашего кода с TradingAgents. Несовместимые изменения API
всё ещё возможны: gate остановит обновление, и правка потребуется в адаптере.
Offline green не доказывает provider availability, качество стратегии, alpha
или live execution. Платный model/provider smoke и forward paper остаются
отдельными этапами исходного плана.

Оба компонента обновляются независимо. В частности, миграция Nautilus v1→v2
может потребовать адаптации `nautilus_probe.py`, окружения и данных; одной смены
тега недостаточно. Текущая проверка native engine должна оставаться зелёной.

## Автоматическая проверка свежести

[Quality](../.github/workflows/quality.yml) проверяет Python-зависимости, lint,
тесты и сборку на Ubuntu и Windows при push и pull request. На Linux он также
проверяет четыре Go-модуля, в том числе гонки данных и сценарии с PostgreSQL 17.
Compose smoke проверяет отказ при неподдерживаемой версии результата, замену и
откат воркера, сохранение заданий и запуск отдельного evaluation-контейнера.
[Сценарий smoke](../deploy/research/smoke.py).

[Проверка совместимости](../.github/workflows/upstream-compatibility.yml)
запускается ежедневно на default branch и вручную. Один job выбирает тег и SHA
последнего стабильного TradingAgents, оба окружения проверяют этот SHA.
При успехе workflow сохраняет candidate locks, `pyproject.toml` и журнал
обновления. Оба workflow используют `contents: read`. Версия в репозитории
меняется локальным updater. [История запусков](https://github.com/Averia17/TradingBot/actions).

Внешние Actions закреплены по commit. Dependabot еженедельно предлагает
обновления, которые проходят Quality. На 14.11.2026 проверены
[checkout v7.0.1](https://github.com/actions/checkout/releases/tag/v7.0.1),
[setup-go v7.0.0](https://github.com/actions/setup-go/releases/tag/v7.0.0),
[setup-uv v10.3.3](https://github.com/astral-sh/setup-uv/releases/tag/v10.3.3),
[upload-artifact v7.0.2](https://github.com/actions/upload-artifact/releases/tag/v7.0.2).

Целевая service architecture и отдельные runtime artifacts описаны в
[architecture](architecture.md). Текущий updater обслуживает dev/build checkout;
этапы упаковки и развёртывания описаны в [service migration](planning/service-migration.md).

Полный upstream suite запускается в Linux CI, а наш adapter suite — на Linux
и Windows. В TradingAgents v0.7.1 upstream исправил предположение теста,
что системный TEMP всегда вне домашнего каталога.
Исторический patch для v0.6.0 сохранён в
[архиве патчей первого M1](../services/research/patches/).
Bootstrap и updater работают с чистыми checkout.
Прежняя локальная ветка `bot/integration` также сохранена отдельно.

При первоначальном M1 `clone --branch v0.6.0` выбирал одноимённую upstream
release ветку с SHA `ff0d0b1`; официальный тег указывает на merge commit
`1394a3f`. Git tree обоих commit идентичен (`8bd10e1`), runtime не изменился.
Текущий bootstrap использует `git init` и точный fetch `refs/tags/...`, поэтому
такое совпадение имён больше не влияет на pin. Рабочий lock содержит SHA тега.

Первичные источники: [TradingAgents releases](https://github.com/TauricResearch/TradingAgents/releases),
[Nautilus releases и v2 transition](https://github.com/nautechsystems/nautilus_trader/releases/tag/v1.231.0),
[GitHub latest release API](https://docs.github.com/en/rest/releases/releases#get-the-latest-release),
[uv lock и frozen sync](https://docs.astral.sh/uv/concepts/projects/sync/),
[GitHub schedule](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule).
