# Чистые upstream и наши адаптеры

Решение: upstream репозитории клонируются без наших изменений. Наш проект хранит
контракты, политики, адаптеры и тесты отдельно. `vendor/` исключён из нашего Git;
публичные URL, release tag и точный commit записаны в `upstream.lock.json`.
Bootstrap воспроизводит эти checkout на другой машине. Fork для этого не нужен.

```text
TradingBot/                         наш Git
  src/tradingbot/adapters/          интерфейсы внешних компонентов
    tradingagents.py               imports/config/context/graph/report boundary
  src/tradingbot/contracts.py       наш стабильный JSON contract и проверки
  src/tradingbot/research.py        общий portfolio arbitration
  src/tradingbot/evaluation.py      отдельная граница Nautilus engine
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

## Обновление

Из корня проекта, PowerShell:

```powershell
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

Кроссплатформенные эквиваленты:

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
может потребовать адаптации `evaluation.py`, окружения и данных; одной смены
тега недостаточно. Текущая проверка native engine должна оставаться зелёной.

## Автоматическая проверка свежести

`.github/workflows/quality.yml` проверяет pull request/push на Ubuntu и Windows.
`.github/workflows/upstream-compatibility.yml` ежедневно и по ручному запуску
тестирует latest stable TradingAgents в одноразовом CI окружении и сохраняет
проверенные candidate lock-файлы. CI не изменяет ветки и не разворачивает бота.
Локальное продвижение выполняется командой updater выше.

Пока у нашего Git нет remote, workflows не работают на сервере. Для активации
нужно разместить проект на GitHub и workflows на default branch. Required
checks/branch protection на сервере пока не настроены. Плановый запуск GitHub
может задержаться; это не обещание немедленной проверки каждого commit.
Подробнее: `.github/README.md`.

Полный upstream suite запускается в Linux CI. На Windows upstream v0.6.0
содержит тест, который ошибочно предполагает, что системный TEMP всегда вне
домашнего каталога. Его больше не патчим и не пропускаем ради зелёного результата;
на Windows проверяем наш adapter suite. Исторический patch сохранён в
`patches/trading-agents/windows-test-isolation.patch`, но не применяется.
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
