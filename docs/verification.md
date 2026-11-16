# История проверок TradingBot

Пути и результаты в этом документе относятся к состоянию репозитория на дату прогона. Текущая структура описана в [архитектуре](architecture.md).

## S4 — Офлайн-образы и замена worker

**16.11.2026.** Локальный `deploy/research/smoke.py` собрал образы Control,
research и evaluation. Стек поднял PostgreSQL, отклонил результат с неизвестной
версией и завершил задания до, во время и после замены worker. При откате
сохранены записи первых двух запусков. Go Control и PostgreSQL оставались
запущенными, ID образа evaluation не менялся. Тестовый кандидат отличался
идентификатором образа и использовал тот же runtime. Результат и manifest
сошлись по версии worker 0.1.0 и SHA TradingAgents `c4544913a14ed3df5f1ed7b9e8483270ce20682a`.
Режим `evaluation` с отключённой сетью подтвердил Nautilus 1.231.0 и протокол
replay `0.1`. Тестовые Compose-проекты и их тома удалены. Hosted CI ещё не наблюдался.

## S5 — Проверенные офлайн-срезы

**16.11.2026.** В PostgreSQL 17 проверены точные суммы, идемпотентность,
конкурентные резервы, stale/expired, сохранение резерва при `unknown`, снятие
при отмене и полная покупка и продажа симулированного инструмента. Через HTTP
на отдельном процессе Control начальные виртуальные $1 000 после покупки
дали $979,50 свободного баланса и 125 raw units. Возврат позиции на тестовой
продаже дал $1 003,20. Состояние и шесть событий прочитаны после перезапуска,
в другой базе подтверждены `pg_dump` и `pg_restore` с теми же суммами и версией.
Эти цены созданы для теста учёта.

Отдельный evaluation runtime прошёл 15 тестов. В хронологическом Nautilus
replay синтетическая круговая сделка дала $1 007,90 против $1 000 cash benchmark
с модельными комиссиями $2,10. Локальный PIT-загрузчик сверил SHA-256 входа и
выбрал доступную к `as_of` ревизию. Сценарии не измеряют доходность проекта.

Два запуска `research-scheduler` с одним расписанием дали HTTP `201`, затем
`200` для одного ID. Отдельный Python-процесс выполнил граф на архивном снимке.
В PostgreSQL остались одно задание, один результат и 12 разрешений. В отчёт
вошла ранняя ревизия отчётности, а более поздняя не попала в контекст.
`paper-replay` проверен отдельным процессом на PostgreSQL. При повторе того же
входа записалось одно исполнение, неопределённый исход оставил резерв в базе.

В Go FIFO-проекция через HTTP удержала себестоимость $20,50 до продажи. После
её записи отчёт показал реализованный результат $3,20 и учтённые комиссии
$0,18 для того же тестового счёта. Версия без будущей продажи и итоговое
состояние проверены отдельно.

Nautilus добавил в нативный cash $1,37 после момента тестовой дивидендной
выплаты, сохранив $1 007,90 до выплаты. Отдельный прогон сверил cash,
buy-and-hold и равную начальную аллокацию при единых котировках, времени
и комиссиях. Совпавший горизонт исключил будущие дивиденды из оценки.

Итоговая проверка среза прошла на PostgreSQL 17 и Linux Docker. Go control
прошёл `-race`, `vet` и сборку. Research прошёл 125 тестов, включая оба
сквозных режима worker. Windows пропустил тест символьной ссылки, проверенный
отдельно в Linux. Evaluation прошёл 30 тестов. Ruff и `git diff --check`
прошли. Остались предупреждения Nautilus о `Timestamp.utcnow`.

## S3c — Общий бюджет вызовов

**16.11.2026.** В отдельной PostgreSQL 17.12 начальный бюджет задания TSLA
составлял 14 вызовов. После commit первого разрешения тестовый прокси потерял
HTTP `201`. Тот же запрос вернул исходную квитанцию с `200`. После commit
второго разрешения worker был принудительно остановлен до получения ответа.
Истечение аренды воспроизвели в тестовой БД. После перезапуска Control новый Python-процесс
завершил граф на оставшихся 12 слотах. SQL подтвердил две записи разрешений
для первой попытки и двенадцать для второй. Платных вызовов было 0.

Интеграционные тесты Go с `-race` и реальной PostgreSQL дополнительно проверили
исчерпание лимита, конкурентные разрешения, отклонение старой аренды и неверного
счётчика результата. Миграция не выдаёт новый бюджет при неизвестном расходе
старой незавершённой попытки. Ранее записанный результат проходит точный replay.
Прошли 103 Python-теста, включая сквозной тест с PostgreSQL, Ruff, а также Go
`vet` и `build`. Остались два предупреждения `Timestamp.utcnow` в Nautilus probe.

## S3b — Python TradingAgents worker

**16.11.2026.** Отдельный Go control сохранил задание TSLA в PostgreSQL 17.12.
`tradingbot worker` запустил настоящий pinned граф TradingAgents с локальной
моделью `scripted-offline-v1` и сохранил результат `0.2`, прочитанный после
перезапуска control. В PostgreSQL подтверждены 12 вызовов тестовой модели,
все без token usage. Манифест, SHA upstream и номер попытки сверены в тесте.
USD billing не измерялся. Внешние поставщики не вызывались.

Для повторения запускается `tests/test_worker_postgres.py` с
`TRADINGBOT_E2E_DATABASE_URL` отдельной пустой PostgreSQL. Версия результата
`0.1` для старого Go fixture worker сохранена.
Прошли 98 Python-тестов, включая сквозной PostgreSQL-тест, Ruff, офлайн-сборка
wheel/sdist и проверка upstream locks. Go control прошёл `-race` с PostgreSQL,
`vet` и сборку. В Nautilus probe остались два предупреждения `Timestamp.utcnow`.

## S3a — Go fixture worker

**16.11.2026.** На PostgreSQL 17.12 проверены отдельные процессы
`control` и `research-fixture-worker`. Локальный HTTP-прокси прервал ответы
после выдачи lease и записи результата. Повтор `claim` с тем же ID вернул
ту же попытку, heartbeat был принят. Повтор `complete` с теми же байтами дал
`200` после потерянного `201`. После перезапуска control результат читался,
в БД остались одна запись результата и одно событие `research_job.completed`.
Результат привязан к выданному снимку и содержит `llm_calls: 0`.

Автоматические тесты с `TRADINGBOT_TEST_DATABASE_URL` также проверяют
периодическое продление lease во время вычисления, отмену просроченной
попытки, отклонение её результата и повторную выдачу с сохранением результата
в новой попытке. Проверка работала с изолированной тестовой БД, без вызовов
LLM и внешних поставщиков рыночных данных.
В модуле `services/control` прошли `go test -mod=readonly -race -count=1 ./...`
с тестовой PostgreSQL, `go vet -mod=readonly ./...`, `go build -mod=readonly ./...`
и `go mod verify`. `gofmt -l` вернул пустой список.

## Milestone 1

Исторический отчёт первоначального M1. Текущий процесс заменён на
[clean upstream + adapters](upstream-updates.md): patch больше не применяется,
а официальный release-tag SHA уточнён до `1394a3f` вместо одноимённой release
ветки `ff0d0b1`. Их Git tree идентичны (`8bd10e1`); исходные runtime файлы те же.

**Date:**04.10.2026. **Result:** первая research implementation готова к запуску на выбранном API provider, offline path проверен. Это не completed autonomous live trader.

### Delivered

1. Cloned two selected official repos; pinned release SHA/versions in `upstream.lock.json`; TA local branch `bot/integration`, upstream remote retained.
2. Python3.12 environment, installed TA0.6.0/Nautilus1.231.0, `uv.lock`, executable bootstrap and buildable package.
3. Decimal financial contracts for marked portfolio, reserved cash, five actions, horizon hours, model estimates, source references and trusted decision IDs.
4. Adapter calls actual `TradingAgentsGraph.propagate(..., portfolio=PortfolioContext(...))`; global structured allocator sees all requested reports and shared cash. No invented expected-return/confidence conversion from a rating.
5. Fail-closed validation: symbol coverage, duplicates, no short, BUY/ADD/SELL/REDUCE consistency, concentration, cash across all buys, no use of unfilled sales, missing/fabricated evidence and invalid typed output.
6. Thread-safe LLM call budget with actual LangChain callback-path check; retries disabled; observed usage tracked, dollar billing explicitly not implemented.
7. Research CLI: `demo`, `analyze`, `doctor`, `schema`, `probe-engine`. Exact schema exported to `shared/trade-intent/schema.json`.
8. Synthetic Nautilus probe uses native cash account, quote events, market orders, fills and positions: TSLA1 and NVDA2 at artificial $100 ask, $700 cash remains. It is not connected to AI intents and not an alpha backtest.

### Verification and root cause evidence

- Our full suite: **29 passed**,2 upstream Pandas warnings. Network disabled by autouse fixtures. Financial boundaries, typed failure, JSON output/overwrite protection, callback cap and native engine are covered. JUnit: `runtime/verification/our.xml`.
- TA stable upstream initial run:1258 passed,1 failed,4 skipped,1 deselected,99 subtests. Failed assertion required cache path outside `Path.home()`. On Windows normal temporary directory is under `C:/Users/User/AppData/Local/Temp`, so assertion tested filesystem layout rather than isolation.
- Fix: assert dedicated suite temp directory parent and `tradingagents-tests-yf-` prefix. No network guard removed, no test skipped or weakened into unconditional pass. Focused isolation suite:9 passed.
- Complete TA suite after patch: **1259 passed,4 skipped,1 deselected,99 subtests**. Three skips are POSIX file-mode tests, one absent optional Bedrock dependency; integration marked test excluded. Warnings from deliberate unknown-model fixtures retained.
- Native Nautilus probe passes; emits two upstream Pandas4Warning messages for deprecated `Timestamp.utcnow` under required pandas3.0.6. No suppression or dependency downgrade; future upgrade compatibility remains a tracked caveat.
- Bootstrap verifies source commit (including annotated-tag dereference) and idempotent patch; component doctor reports versions0.1.0/0.6.0/1.231.0.
- Wheel/sdist built with `uv build --no-sources`; synthetic demo archive exists in `runtime/m1-demo`; engine result in `runtime/m1-engine-probe.json`. Runtime directories ignored by Git. `ruff check`, `ruff format --check` pass; `uv pip check` confirms102 installed packages compatible.

### Review boundaries

Reviewed correctness, simplicity, dependency compatibility, external-input handling and resource limits using code-review skill. This is a self-review of the first small research component, not an independent production security audit. No privileged wallet/API execution capability exists; schema permits only research. AI estimates aren't calibrated, reports aren't verified intraday PIT, actual quote costs unavailable, SQL reservations/storage not implemented.

Common model API keys are **not configured** in the inspected environment. No paid analysis was attempted; no expected alpha/real recommendations were manufactured. `analyze` entry point is wired to real upstream/provider APIs but awaits credentials and an actual model compatibility test. Store credential locally; never provide wallet seed.

### Next vertical slice

Run bounded actual TSLA/NVDA analysis with a locally configured provider; verify strict portfolio output, actual call/token counts and fail-closed schema compatibility. Then connect persisted intents and archived PIT inputs to chronological Nautilus portfolio replay with costs/corporate actions. Do not write DEX routing or wallet signing before these are verified. Native TA fixed-window reflection remains a research baseline, not realized net trade memory.

## Clean upstream

### 2026-11-14

- В задаче проверки кандидата на Linux и Windows зафиксирован exit 2.
  Детальные логи недоступны анонимно. Та же команда updater локально
  воспроизвела Ruff `I001` в `src/tradingbot/contracts.py` после успешного
  `doctor`. [GitHub Actions run #41](https://github.com/Averia17/TradingBot/actions/runs/43590785716).
- После исправления импортов updater установил TradingAgents `v0.7.1`
  (`c4544913a14ed3df5f1ed7b9e8483270ce20682a`). Прошли dependency check,
  `doctor`, Ruff, 73 теста проекта и сборка wheel/sdist. Nautilus `v1.231.0`
  остаётся последним стабильным релизом. Оба checkout прошли `verify`.
- Исходный suite TradingAgents на Windows: 1407 passed, 6 skipped,
  1 deselected. Локальный JUnit — `runtime/verification/upstream-windows.xml`.
  Hosted запуск с этими изменениями ещё не проверен.
- После проверки Python/Go wire schema и нормализации денежных строк полный
  набор Python прошёл с результатом 83 passed. Остались два предупреждения
  Nautilus о deprecated `Timestamp.utcnow`.

### 2026-10-04

Уточнение 05.10.2026: local GitHub remote уже настроен. Выполнение hosted Actions
и server branch protection не проверены; прежнее указание об отсутствии remote
ниже исправлено. Counts и результаты runtime-проверок относятся к 04.10.2026.

Проверены изменения процедуры обновления и границы TradingAgents adapter.

- TradingAgents release `v0.6.0`, exact tag commit
  `1394a3f72aa4393e1a98f51b382434c4b4c2d972`; Nautilus `v1.231.0`,
  `27a8e54e7ac3c57d6cbf8891f0283dfbaee97317`.
- Оба checkout имеют правильный `upstream`, соответствуют manifest HEAD,
  не имеют tracked/untracked изменений; patches не применяются.
- `bootstrap.ps1`, `update-upstream.ps1 -CheckOnly` и настоящая команда
  `update-upstream.ps1` успешно выполнены. На момент запроса latest TA = v0.6.0.
- Updater выполнил разрешение/установку зависимостей, doctor, Ruff,
  полный suite и build. Отчёт:
  `runtime/upstream-updates/f213f05e-7376-4521-b6b4-f3c72e95ccf9.json`.
- После дополнительной проверки возобновления bootstrap: полный suite
  **69 passed**, Ruff без ошибок, `uv pip check` — 102 packages compatible,
  `uv lock --check` успешен, wheel/sdist собраны.
- 24 updater tests используют настоящие временные локальные Git репозитории:
  annotated tags, совпадающие имена ветки/тега, dirty/неверный remote/HEAD,
  retag, неизвестный SHA кандидата, static/dynamic version, rollback файлов и
  окружения, ошибка восстановления, ignored collision, concurrent lock,
  nonstable release refusal и безопасное возобновление пустого checkout.
- Adapter suite запускает настоящий upstream graph с scripted offline models;
  проверены свободный cash $850 и позиция TSLA 1.25 в фактических prompts.
  Все наши тесты запрещают socket/curl network; LLM кредиты не расходуются.
- Остались два существующих предупреждения deprecated `Timestamp.utcnow`
  из Nautilus во время native-engine probe; тест проходит, upstream не патчим.
- Workflows проверены YAML parsing и actionlint v1.7.12. Hosted GitHub runs и
  server branch protection здесь не проверялись; их статус не следует из YAML.
- Полный неизменённый upstream suite на Windows здесь не объявляется зелёным:
  его известная TEMP/HOME assumption описана в инструкции. Linux CI запускает
  стандартный изолированный suite дополнительно; это ещё предстоит серверной
  проверке. Real paid provider/live/alpha не проверены этим изменением.

Исходная branch/tag путаница уточнена по `git ls-remote`: release ветка
`refs/heads/v0.6.0` указывает на `ff0d0b1`, тег `refs/tags/v0.6.0` — на
`1394a3f`. Trees одинаковы `8bd10e10ee9ba1dca4441596e123d3809e433480`.
Текущее клонирование через init + exact-tag fetch устраняет неоднозначность.

Процедура: [upstream updates](upstream-updates.md).
