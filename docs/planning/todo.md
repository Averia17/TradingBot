# Implementation backlog — 04.10.2026

**Backlog P0–P8.** Текущий статус и порядок работ — в [основном плане](plan.md).
Галочка означает выполнение полного acceptance ticket. Пути в ранних задачах
отражают исходный план и уточняются по [структуре Go/Python](service-migration.md).

**Уже есть:** P1-01 pinned setup и offline verification. P1-02/04/05 и P2-04
частично покрыты research DTO, общими Go/Python fixtures и schemas, call cap,
usage, portfolio adapter и native-engine probe. Полное acceptance требует
проверок provider outputs, billing, PIT, ledger и corporate actions.
Исторические результаты сохранены в [отчёте M1](../verification.md#milestone-1).

## Phase 0 — Verification

Архитектурное уточнение 05.10.2026 и Go migration tickets S1–S6 вынесены в
[service-migration.md](service-migration.md). Новый backend пишем на Go,
Python сохраняет upstream research/evaluation. Исходные acceptance gates ниже
остаются обязательными; наличие новой архитектуры их не завершает.
Scheduler, jobs, control и ledger относятся к Go-сервису `services/control/`.
Research и текущая диагностика Nautilus находятся в `services/research/`. Пути
для ещё не реализованных компонентов обозначают предполагаемое размещение.

### [ ] P0-01 — Account-specific eligibility dossier

- **Goal:** выбрать разрешённые research и execution paths.
- **Context:** EU retail не равен direct qualified issuer client; конкретная страна UNKNOWN.
- **Files/components:** `docs/eligibility.md`, `docs/provider-access.csv`.
- **Acceptance:** для страны резидентства и категории владельца зафиксированы дата и версия условий, доступ к Jupiter API и вторичному рынку Solana xStocks, допуск к конкретным сериям, ввод и вывод фиата/USDC и требования выбранных провайдеров. Отдельно отмечены права на прямой mint/redeem у эмитента, альтернативные xChange/Ondo/брокер и ограничения каждого пути. Пробелы остаются `UNKNOWN` и блокируют затронутый live-маршрут. Сценарий Беларуси хранится отдельно.
- **Tests:** каждая разрешённая операция имеет evidence для конкретного аккаунта или владельца. Успешная публичная котировка подтверждает только доступ к источнику котировки.
- **Dependencies:** нет; окончательный live результат требует конкретной страны и onboarding.

### [ ] P0-02 — Pin upstream and license manifest

- **Текущий прогресс:** сверены точные теги, коммиты, даты релизов и лицензии upstream и SDK локального контура. Для распространения контейнеров остаётся проверить комплект лицензионных материалов. [Реестр пинов и лицензий](../upstream-updates.md#пины-и-лицензии).
- **Goal:** воспроизводимый состав reuse.
- **Context:** версии TA/Nautilus берём из `services/research/upstream.lock.json`. Дополнительные SDK требуют pin и license review.
- **Files/components:** `docs/upstream-updates.md`, `services/research/upstream.lock.json`.
- **Acceptance:** immutable SHAs TA/Nautilus/SDK; release date, license, patch policy, unresolved compatibility; нет proprietary ATLAS prompts в поставке.
- **Tests:** referenced SHAs/files существуют; license identifiers/conditions сверены с LICENSE.
- **Dependencies:** нет.

### [ ] P0-03 — Data rights and timestamp sample

- **Текущий прогресс:** по публичным источникам сверены время приёма и исправления отчётности SEC, пример пересмотра BLS, ограничения FRED API и форматы потенциального рыночного источника. [Реестр источников и открытые условия](../research/SOURCES.md#11-источники-для-point-in-time-архива--18112026).
- **Goal:** выбрать допустимые источники data/news/actions.
- **Context:** бесплатный demo feed не доказанный production PIT dataset.
- **Files/components:** `docs/data-contracts.md`, `services/research/tests/fixtures/data_samples/`.
- **Acceptance:** по ценам/объёмам underlying, календарю, отчётности, новостям и корпоративным действиям есть перечень источников, права использования и хранения, покрытие, время доступности каждой версии записи, правила свежести и бюджет или `UNKNOWN`. Для новостей сохраняются исходная публикация, первое наблюдение и исправления. Для отчётности сохраняются фактическое время публикации и последующие поправки.
- **Tests:** sample различает `event_time`, `available_at`, `ingested_at` и ревизию. Запись из будущего, недоступная ревизия или просроченный обязательный источник блокируют использование в анализе.
- **Dependencies:** нет.

### [ ] P0-04 — Asset registry evidence

- **Goal:** избежать ticker/mint/ratio путаницы.
- **Context:** wrapper legal terms и Scaled UI различны.
- **Files/components:** `data/manifests/assets.json`, `docs/assets.md`.
- **Acceptance:** начать с NVDA и TSLA. Для NVDAx/TSLAx и settlement USDC сверить официально опубликованный Solana mint и on-chain mint account, включая token program, `decimals` и extensions. Для xStocks записать issuer, коэффициент raw→UI→акция с временем действия, применимые условия/ограничения, источники и версию записи. Остальные кандидаты включаются только после такой же сверки. Допуск BUY/SELL хранится отдельно от идентичности токена.
- **Tests:** совпадающие тикеры с разными mint, подменённый program, неизвестное extension и изменение multiplier. Воспроизведение размера сделки использует registry version на момент решения.
- **Dependencies:** P0-01; public evidence можно собрать заранее.

### [ ] P0-05 — Quote-study protocol

- **Текущий прогресс:** `paper-quote-study` фиксирует исходную сетку слотов и хеш локальных наблюдений. В отчёте отдельно видны ответы, отказы и пропуски. Группы различаются по инструменту, маршруту, стороне, размеру и сессии. Проверено на синтетическом архиве.
- **Goal:** получить сравнимые cost observations.
- **Context:** ни одна U-cell main plan пока не quote evidence.
- **Files/components:** `experiments/quote-study/protocol.md`, `contracts/quote-observation.json`.
- **Acceptance:** протокол задаёт пары USDC→допущенный xStock и обратно для того же mint, сетку размеров с учётом минимального ордера и лимита пилота, матрицу regular/extended/off-hours, формат timestamp/ref/bid/ask/input/output/min-output/TTL/fees, режим котировки и лимиты запросов. Первый срез — NVDAx/TSLAx, расширение — до восьми допущенных активов и пяти размеров. Учитываются неуспешные ответы и отказы. Период наблюдений — не менее 20 торговых дней.
- **Tests:** различаются synthetic/indicative/используемая котировка, её расчётный output и подтверждённый fill. Пропуски, неизвестные компоненты расходов и устаревший reference явно маркируются.
- **Dependencies:** P0-01, P0-04 для сбора. Схему наблюдения и сетку размеров можно подготовить заранее.

### [ ] P0-06 — Register economics and decision gates

- **Goal:** зафиксировать criterion до просмотра результатов.
- **Context:** success = net risk-adjusted excess, не число сделок.
- **Files/components:** `experiments/protocol.md`, `docs/budgets.md`.
- **Acceptance:** до просмотра результатов зафиксированы базовая валюта, доступный основной benchmark с total return, пассивный и детерминированный контроль, NAV на весь выделенный капитал, статьи переменных и регулярных затрат, допустимая просадка, метод оценки неопределённости и правила `go/hold/stop`. Для входа зафиксированы версия сценарной модели, минимальный запас над двусторонними затратами и fallback при отсутствии калибровки. Владелец отдельно утверждает финансовые параметры для real mode в P7-01.
- **Tests:** воспроизводятся условная сделка $400 с +5% и заданными расходами $0.60 + $0.50, зеркальное движение −5% при тех же расходах и годовой итог $1 000 + $180 − $20 − $30. Для сравнения показываются номинальное превышение и отдельная оценка с поправкой на риск. Доступность QQQ или заменяющего инструмента проверена отдельно.
- **Dependencies:** P0-03, P0-05.

## Phase 1 — Local baseline

### [ ] P1-01 — Pinned TradingAgents offline baseline

- **Goal:** первый coding artifact без wallet.
- **Context:** release main не плавающая dependency.
- **Files/components:** `services/research/pyproject.toml`, `services/research/uv.lock`, `services/research/upstream.lock.json`, `services/research/tradingbot/`, baseline artifacts, clean upstream checkouts.
- **Acceptance:** setup documented; installed version matches SHA; upstream checks recorded; single-symbol fixture run archived with hashes; no execution credentials.
- **Tests:** fresh-environment offline replay; network disabled fixtures; upstream test outcome report.
- **Dependencies:** P0-02; P0-03 sample.

### [ ] P1-02 — Typed intent and snapshot contracts

- **Goal:** заменить ambiguous sizing/action outputs.
- **Context:** upstream typed PM не готовый executable order.
- **Files/components:** `contracts/`, `services/research/tradingbot/contracts.py`, `services/control/internal/contract/`, fixtures. Held-wrapper identity расширяется отдельным контрактом перед paper/live.
- **Acceptance:** пять действий, target/delta/units, горизонт, expiry, триггер пересмотра, условия опровержения тезиса и ссылки на источники. Для риск-увеличивающих предложений — диапазон сценариев и ожидаемых изменений с явным источником оценки и статусом калибровки. Обработка плохих данных, истёкшего снимка и неизвестного бюджета завершается структурированной причиной отказа.
- **Tests:** неизвестное действие, свободный текст, NaN, несовместимые единицы, stale snapshot, ссылки на отсутствующее evidence, invalid probabilities при их наличии и SELL без идентифицированной позиции.
- **Dependencies:** P1-01. Для research проверяем на fixtures и публичных данных registry. Для работы с реальными held instruments требуется P0-04 и проверка конкретного account path.

### [ ] P1-03 — Immutable snapshot tool boundary

- **Goal:** tools возвращают только разрешённый frozen context.
- **Context:** upstream day cutoff недостаточен для intraday.
- **Files/components:** `services/research/tradingbot/snapshots.py`, `services/research/tradingbot/tools.py`, `data/manifests/`.
- **Acceptance:** as_of timestamp, source/available_at/hash; no agent live vendor fallback during replay; macro reused by snapshot.
- **Tests:** late news refused, content hash mismatch refused, offline tool replay complete.
- **Dependencies:** P1-02, P0-03.

### [ ] P1-04 — Usage and billing instrumentation

- **Текущий прогресс:** локальный worker сохраняет исход вызова и доступные счётчики в PostgreSQL по ID разрешения и попытки. `research-usage-export` выгружает все разрешения, включая пропущенные ответы после сбоя. `research-costs` применяет закреплённые ставки USD и переносит оценки только в синтетический ledger затрат. Платёжные квитанции и реальная совместимость моделей остаются открытыми.
- **Goal:** измерить full decision p95 cost.
- **Context:** aggregate tokens без rates/cache/retry неполны.
- **Files/components:** `services/research/tradingbot/llm_usage.py`, [контракт журнала](../../contracts/research-llm-usage.schema.json), `experiments/baseline/costs/`.
- **Acceptance:** model/role/rate date/input/cache/output/reasoning/retry/tool cost; cap interrupts new calls; provider-supported schema model identified.
- **Tests:** cache subset not double-counted; retry billed; missing usage flagged; cap enforced.
- **Dependencies:** P1-01, P0-06.

### [ ] P1-05 — Portfolio-aware batch allocation adapter

- **Goal:** несколько symbol proposals учитывают один cash ledger.
- **Context:** static single-ticker portfolio input не portfolio optimizer.
- **Files/components:** `services/research/tradingbot/allocation.py`, `services/research/tradingbot/agents_adapter.py`.
- **Acceptance:** один portfolio allocator видит доступный cash, резервы, held positions и pending orders. Оценка ожидаемого изменения отделена от basis и проверяется против ожидаемых затрат входа/выхода и буфера модели. Размер ограничивается политикой, свободным cash, ликвидностью и концентрацией по инструменту/сектору/эмитенту. Сценарии и стоимость сохраняются вместе с причиной BUY/ADD или осознанного пропуска.
- **Tests:** видимый upside при отрицательном net edge, некалиброванная уверенность, пять предложений к одному cash, одновременные BUY и ожидаемая выручка SELL, конфликт по концентрации, `REDUCE`/`HOLD` и пример с привязкой к цене входа.
- **Dependencies:** P1-02, P1-03 и правила входа из P0-06. На раннем офлайн-этапе
  ставки берутся из fixtures и помечаются как сценарные. Для измеренной оценки
  затрат требуется P3-04.

### [ ] P1-06 — Baseline replay and variance report

- **Goal:** измерить runtime correctness/cost до расширения.
- **Context:** temperature zero не гарантирует повторяемость.
- **Files/components:** `experiments/baseline/report.md`, `services/evaluation/replay.py`.
- **Acceptance:** fixed fixture corpus, bounded repeat sample, valid output/evidence/tool rounds/latency/cost report; failure reasons preserved.
- **Tests:** each run reconstructable; invalid intent never creates order; report separates synthetic/actual billed calls.
- **Dependencies:** P1-03, P1-04, P1-05.

## Phase 2 — Historical evaluator

### [ ] P2-01 — PostgreSQL event ledger and migrations

- **Текущий прогресс:** Go paper ledger хранит виртуальные счета, частичные и полные fills, резервы и журнал. Проверены перезапуск, восстановление копии, миграция исполнений из 0.1 и точные суммы через HTTP.
- **Goal:** authoritative cash/intent/event identifiers.
- **Context:** mutable Markdown не accounting ledger.
- **Files/components:** `services/control/internal/ledger/`, PostgreSQL ledger migrations, `contracts/events.json`.
- **Acceptance:** events/positions/cashflows/intents/orders/reservations, unique idempotency keys, decimal precision, migration/rollback/backup policy.
- **Tests:** conservation, duplicate event rejection, transactional rollback, migration on clean database.
- **Dependencies:** P1-02.

### [ ] P2-02 — SEC acceptance and vintage ingestion

- **Company Facts:** локальный импорт связывает точные значения US GAAP
  с проверенным accession. Показатели новой ревизии видны только после
  документированного получения Submissions и Company Facts, а поправки
  сохраняют отдельный accession.
- **Текущий прогресс:** локальный импорт проверяет SHA-256 сохранённых ответов EDGAR и ALFRED, сохраняет время приёма, accession, поправки, даты винтажей и десятичные значения. Архив использует зафиксированное в манифесте локальное время получения. Сбор, покрытие и права на реальные данные ещё предстоит подтвердить.
- **Goal:** point-in-time fundamentals/macro.
- **Context:** filed date/revised latest series недостаточны.
- **Files/components:** `services/research/tradingbot/ingest/sec.py`, `fred.py`, `data/manifests/`.
- **Acceptance:** accession acceptance/availability/revision; ALFRED vintage and release timing; fair-access cache/backoff.
- **Tests:** afternoon filing unavailable in morning; amendment cannot alter earlier snapshot; vintage fixture.
- **Dependencies:** P1-03, P0-03.

### [ ] P2-03 — Prices, news and historical universe archive

- **Идентификация в исследовании:** отбор `0.3`, задание `0.2` и результат
  `0.3` связывают тикер с устойчивой бумагой на заданном срезе. Проверяются
  пины владельца, действующий листинг и источники. Виртуальные входы
  поддерживают ту же привязку. Пересмотр позиции и финансовая лента
  Nautilus требуют своих версий контракта.
- **Текущий прогресс:** локальный импорт связывает raw OHLCV, корпоративные действия, правки новостей и события листинга с SHA-256 и временем получения. Проверена история двух бумаг с одним тикером. Отбор 0.2 требует устойчивый ID в снимке universe. Права и покрытие реального исторического архива остаются неизвестны.
- **Goal:** исключить survivorship и future adjustment.
- **Context:** latest Yahoo adjusted bars и current index members недостаточны.
- **Files/components:** `services/research/tradingbot/ingest/market.py`, `news.py`, `universe.py`, raw object manifests.
- **Acceptance:** raw bars/actions, first-seen/corrections, listings/delistings; missing licensed history explicitly blocks affected claims.
- **Tests:** delisted security retained; future split doesn't rescale historic cash fill; news correction cutoff.
- **Dependencies:** P0-03, P1-03.

### [ ] P2-04 — Nautilus compatibility spike

- **Текущий прогресс:** изолированный pinned Nautilus runtime выполняет синтетический multi-asset replay и бенчмарки, включая денежные дивиденды и сплиты акций. Токенные множители и дробные компенсации требуют отдельной модели.
- **Goal:** подтвердить reuse engine до большого adapter.
- **Context:** token multiplier/actions не доказаны engine defaults.
- **Files/components:** `services/evaluation/engine.py`, `services/research/tests/fixtures/actions/`, `docs/adr/evaluator.md`.
- **Acceptance:** pinned engine creates chronological two-asset NAV with cash/actions; supported gap/custom adapter scope recorded; fallback decision if failed.
- **Tests:** known roundtrip, dividend/split/multiplier, fractional decimals and fee currency.
- **Dependencies:** P0-02, P2-01.

### [ ] P2-05 — Next-executable-fill portfolio replay

- Календарный режим синтетической ленты проверяет известные к поступлению
  окна торгов, закрытия и приостановки. Свежая котировка после открытия
  нужна для следующего исполнения. Те же ограничения применяются к оценке
  и бенчмаркам. Подтверждение календаря реальной площадки остаётся открытым.
- **Текущий прогресс:** собственный replay читает сохранённые research-решения PostgreSQL или запускает закреплённый офлайн-граф на каждом историческом снимке из состояния Nautilus. Лента и архив фиксируются хешами. Строгая сверка проверяет cash и позиции к каждому снимку и выявляет его устаревание до исполнения. Производный replay даёт дневной NAV и бенчмарки. Для проверки стратегии на рынке нужны лицензированный PIT-архив, реальные параметры доступности и наблюдаемые расходы.
- **Устойчивый ID:** `research-replay` и `walk-forward` проверяют решения `0.2` с отдельным ключом Nautilus на каждую бумагу. Дневные `replay-evidence` и `walk-forward-evidence` закрепляют исходные данные и реестр. Они проверяют котировки на каждой отсечке и сравнивают NAV с эталонами по тому же расписанию.
- **Goal:** заменить ticker-first rating evaluation.
- **Context:** inference completion предшествует fill.
- **Files/components:** `services/evaluation/replay.py`, `fills.py`.
- **Acceptance:** global timeline, latency/calendar, orders/partial/cancel/expiry/halts, dynamic portfolio snapshot; no same-close hindsight fill.
- **Tests:** simultaneous symbols/cash constraint, delayed decision, holiday/DST, partial fill.
- **Dependencies:** P2-04, P2-02, P2-03.

### [ ] P2-06 — Deterministic and investment benchmarks

- **Ребалансировка:** `benchmarks --rebalance-at` добавляет равновзвешенный
  расчёт на Nautilus. Состав начального universe фиксируется, а размер
  покупок зависит от средств после фактически исполненных продаж.
  Хеш расписания, комиссии и свободный кэш входят в отчёт.
  Дневные точки replay и walk-forward используют то же расписание из
  замороженного протокола и только данные к своим cutoff.
- **Текущий прогресс:** синтетический native replay строит cash, buy-and-hold, equal-weight и трендовый long/cash по одному инструменту. Тренд использует зафиксированные lookback и максимальный пропуск котировок. Рыночные затраты и доступность эталонов ещё предстоит подтвердить.
- **Goal:** сопоставить сложный AI с простыми стратегиями.
- **Context:** nominal return не alpha.
- **Files/components:** `services/evaluation/benchmarks.py`, `reports.py`.
- **Acceptance:** cash/B&H/equal-weight/trend; accessible vs theoretical flags; same base currency and funding/cost model.
- **Tests:** benchmark NAV fixtures, net dividends, rebalancing fees, inactive capital.
- **Dependencies:** P2-05, P0-06.

### [ ] P2-07 — Leakage and statistical protocol audit

- **Текущий прогресс:** замороженный протокол проверяет границы выборок, пересечение меток, эмбарго, доступность входов и даты NAV. Он фиксирует входной план и ленту walk-forward до анализа, а отчёт хранит хеш шагов. На парной дневной избыточной доходности воспроизводятся интервалы блочного bootstrap с поправкой на зарегистрированные сравнения. Синтетический набор и неизвестная история обучения модели маркируются в отчёте.
- **Goal:** честно классифицировать retrospective evidence.
- **Context:** LLM weights могут содержать test outcomes.
- **Files/components:** `docs/leakage-register.md`, `services/evaluation/statistics.py`, experiment split manifest.
- **Acceptance:** purge/embargo by horizon, frozen splits, model cutoff unknown label, blocked CI/multiple-testing plan, no clean-OOS claim без evidence.
- **Tests:** future observation/lesson negative controls; overlaps removed; synthetic correlated sample CI sanity.
- **Dependencies:** P2-05, P2-06.

## Phase 3 — Costs and quotes

### [ ] P3-01 — Read-only quote adapters

- **Goal:** собирать permitted quotes без подписи.
- **Context:** current Swap v2/issuer schemas нужно version pin.
- **Files/components:** `services/control/internal/venues/quote/`, generated API clients, recorded fixtures.
- **Acceptance:** первый кандидат — Jupiter для Solana USDC↔допущенные xStocks.
  Сбор включается после P0-01. Зафиксированы версия API и ответы, raw amounts,
  route/время/expiry/fee/ref/net output и отказы. Клиент использует методы
  получения котировок и работает без доступа к signer или signed transactions.
  Каждый дополнительный источник требует собственного подтверждения доступа.
- **Tests:** актуальная записанная схема, неверный mint/side/amount, stale, soft vs гарантии конкретного режима, minimum, `429`, неполный ответ и отсутствие подписи/submit в клиенте котировок.
- **Dependencies:** P0-01, P0-04, P0-05.

### [ ] P3-02 — Execute quote-study collection

- **Текущий прогресс:** офлайн-сводка считает покрытие, задержку и стоимость синтетических котировок в одну сторону. P50/P95/P99 доступны при достаточном числе нормализованных ответов в каждой группе. Наблюдения с реальной площадки, торговый календарь и фактический бюджет остаются открытыми.
- **Goal:** заполнить U matrix measured observations.
- **Context:** public schedule не actual spread/size impact.
- **Files/components:** `experiments/quote-study/`, quote SQL table, collection job.
- **Acceptance:** покрытие и доля отказов по активу/размеру/стороне/сессии согласно P0-05, latency и all-in bps p50/p95/p99 вместе с числом наблюдений. Отдельно фиксируются `no_quote`, `expired`, `below_minimum`, `not_permitted` там, где применимо. Соблюдены лимиты запросов и правила получения hard quotes провайдера.
- **Tests:** clock/reference alignment, duplicates, outage/rejection retention, dataset manifest hash, отдельный отчёт о малочисленных и непокрытых ячейках.
- **Dependencies:** P3-01.

### [ ] P3-03 — Share-equivalent routing comparison

- **Текущий прогресс:** `paper-quote-compare` проверяет версию коэффициента и точную идентичность инструмента, размер, срок и settlement FX. Синтетические сравнения не подтверждают реальные котировки, asset registry и доступность площадки.
- **Goal:** корректно сравнить wrappers/held SELL.
- **Context:** token amounts разных issuers не взаимозаменяемы.
- **Files/components:** `services/control/internal/venues/normalize/`, `services/evaluation/quote_compare.py`.
- **Acceptance:** ratio/decimals/reference normalization; sell only held instrument; chain/settlement/issuer constraints precede cost ranking.
- **Tests:** wrong-mint best-price trap, changed multiplier, fee embedded, USDon≠USDC.
- **Dependencies:** P3-01, P0-04.

### [ ] P3-04 — Fee/impact/failure cost replay

- **Текущий прогресс:** отчёт с замороженным cost ledger воспроизводит execution-net и operating-net NAV, сохраняя неизвестные расходы. Затраты вычитаются после симуляции. Измеренные chain, impact и failure costs нужны для полной фазы.
- **Goal:** evaluator uses full economic costs.
- **Context:** nominal 0-fee claims hide spread/gas.
- **Files/components:** `services/evaluation/costs.py`, `services/control/internal/venues/costs/`.
- **Acceptance:** measured quote outputs + explicit fees, failed tx/priority/FX/issuer drag, rent locked separately, sensitivity when historical quotes absent.
- **Tests:** no double-counting; net-output fixture, high-priority fee, depeg, refunded rent.
- **Dependencies:** P2-05, P3-02, P3-03.

### [ ] P3-05 — Cheap scorer with budgeted candidate selection

- **Текущий прогресс:** PIT-отбор по закреплённому stockstats ROC, допуску, свежести, обороту и спреду готовит durable research-задание в пределах явно заданных слотов. После активации суточного лимита PostgreSQL ограничивает общее число разрешений для всех заданий и повторных попыток. Проверены конкурентные запросы, перезапуск и смена суток UTC. Платные расходы и лимиты по валюте остаются открытыми.
- **Goal:** не анализировать500 equities full debate ежедневно.
- **Context:** universe prefilter, не замена AI trade decision.
- **Files/components:** existing Qlib workflow/config adapter либо existing TA indicator-ranking adapter, `services/research/tradingbot/universe/scorer.py`, `config/universe.yaml`.
- **Acceptance:** reused feature/ranking implementation, no new screener framework; permitted assets, PIT indicators/catalysts/quote costs, configurable top-K, event reviews for holdings; feature parameters frozen.
- **Tests:** inaccessible/stale asset excluded; holdings event prioritized; max daily analyses respected.
- **Dependencies:** P2-03, P3-02, P1-04.

### [ ] P3-06 — Broker/token/OPEX economic report

- **Goal:** выбрать cheapest viable path либо stop.
- **Context:** $1k fixed-cost hurdle велик.
- **Files/components:** `experiments/economics/report.md`, `cashflow.csv`.
- **Acceptance:** сравнить основной DeFi-маршрут с доступным брокером на сопоставимом underlying, времени и валюте. Включить buy/sell, FX/on-/off-ramp, расходы решений без исполнения и неудачных отправок, наблюдаемые LLM/data/infra расходы, резерв SOL/ATA и простой cold capital. Брокер служит измеримым сравнением и требует отдельного решения при смене DeFi-сценария. Пробелы в ценах, доступе и capacity явно указаны.
- **Tests:** cashflow math, rates units/bps, переменные расходы входят ровно один раз, ATA rent и возврат учтены раздельно, расходы периода сохраняются при нуле исполнений.
- **Dependencies:** P3-04, P3-05, P2-06.

## Phase 4 — Memory/agent evaluation

### [ ] P4-01 — SQL decision and outcome memory

- **Журнал уроков:** PostgreSQL хранит факт закрытия по исходному SELL,
  ID решения, версию счёта, источник точных сумм и момент доступности.
  Отзыв и восстановление создают события с новой ревизией. Чтение по
  cutoff показывает историю, которую можно было видеть к этому моменту.
- **Goal:** durable PIT memories без vectors.
- **Context:** intraday decisions не ticker/date unique.
- **Files/components:** `db/migrations/002_memory.sql`, `services/research/tradingbot/memory/store.py`.
- **Acceptance:** decision/outcome/lesson IDs; created/available/label_end; immutable original records; versioned correction events.
- **Tests:** multiple same-day decisions; no lesson before availability; replay independent of upstream rotation.
- **Dependencies:** P2-01, P2-05.

### [ ] P4-02 — Horizon-aware actual and shadow outcome labels

- **Текущий прогресс:** отчёт по открытию связывает фактические FIFO-исполнения со сроком тезиса. Открытый остаток требует подходящей оценки выхода на конец срока. Для отклонённого симулятором BUY/ADD отдельный отчёт проверяет сохранённую отмену и считает контрфактуал по хешированным BUY/SELL наблюдениям. Проверены поздние продажи и опциональный эталон. Отказы без записи исполнения и наблюдаемые рыночные расходы остаются открытыми.
- **Goal:** не считать5-day raw return реальным P&L.
- **Context:** hours/months, partial exits, counterfactuals различны.
- **Files/components:** `services/research/tradingbot/memory/outcomes.py`.
- **Acceptance:** executed net cashflow, open horizon marks, benchmark return; separate shadow rejection label; execution vs signal failure.
- **Tests:** month thesis not settled after5days, partial exit, no future benchmark labels, shadow not actual profit.
- **Dependencies:** P4-01, P3-04.

### [ ] P4-03 — Bounded factual retrieval and reflection

- **Версия памяти:** экспорт `--durable-lessons` связывает факты с активным
  событием журнала к cutoff из одного снимка PostgreSQL. После отзыва
  следующие архивы сохраняют исход FIFO без отозванного отражения.
- **Текущий прогресс:** `paper-memory` переносит реализованные FIFO-исходы со ссылками на события до cutoff. Python проверяет scope и ограничивает объём фактов. Вариант `factual_reflection` может приложить производную метку влияния комиссии выхода с точными суммами, хешем и ID источника. Вклад памяти в рыночный результат остаётся открытым.
- **Goal:** полезный auditably sourced context.
- **Context:** reflection может усилить luck/anchoring.
- **Files/components:** `services/research/tradingbot/memory/retrieval.py`, `reflection.py`.
- **Acceptance:** уроки ссылаются на сверенные фактические или явно помеченные
  симулированные исходы и становятся доступны после `available_at`. Извлечение
  учитывает тикер, сектор, режим и лимит контекста. Неуспешные решения и расходы
  сохраняются вместе с удачными. Память не изменяет неизменяемые события,
  конфигурацию стратегии или подписанную политику.
- **Tests:** будущий вывод и запись с неверным источником исключаются,
  инъекция из памяти не получает полномочий, лимит контекста действует.
- **Dependencies:** P4-02, P1-04.

### [ ] P4-04 — Agent/memory paired ablations

- **Текущий прогресс:** закреплённые варианты полного графа, без памяти, с одним аналитиком и с фактической меткой выполняются по одному плану. Протокол требует полный набор сравнений с базовой конфигурацией и общий лимит вызовов. Синтетические прогоны дали одинаковую историю NAV и измеренный расход офлайн-вызовов. Рыночный вклад и оплаченные затраты неизвестны.
- **Goal:** измерить incremental benefit и расходы.
- **Context:** complexity не доказательство alpha.
- **Files/components:** `experiments/ablations/`, `services/evaluation/ablations.py`.
- **Acceptance:** single-agent/no-memory/factual/reflection/full graph; same inputs/cost envelope; results and all variants retained; semantic retrieval только отдельный challenger.
- **Tests:** shared dataset hashes/splits; prompt variations registered; paired CI and billed cost.
- **Dependencies:** P4-03, P2-07, P3-06.

### [ ] P4-05 — Freeze strategy and forward protocol

- **Текущий прогресс:** локальная фиксация связывает точные байты файлов всех стратегий с замороженным протоколом сравнения. Проверка по сохранённому хешу выявляет изменения файлов и состава вариантов. Внешняя дата регистрации, фактическое применение версии при перспективном прогоне и выбор победителя остаются открытыми.
- **Goal:** выбрать simpler winner и зафиксировать prospective experiment.
- **Context:** ретроспективная optimization не clean evidence.
- **Files/components:** `experiments/forward/protocol.md`, `config/frozen-strategy.yaml`.
- **Acceptance:** exact model IDs/prompts/risk/memory/scorer hashes; primary comparison, change/challenger policy и insufficient-power outcome.
- **Tests:** config tampering starts new run; no hindsight variant removal.
- **Dependencies:** P4-04, P0-06.

## Phase 5 — Paper operation

### [ ] P5-01 — Paper scheduling over durable research jobs

- **Фоновый запуск:** профиль `paper-entry` обрабатывает окна закреплённого
  плана и повторяет постановку задания из постоянного журнала после
  пересоздания контейнера. Сбой вызова Go для одного окна допускает обработку
  остальных окон.
- **Текущий прогресс:** календарь, локальный отбор для входа, архивные события и пересмотры позиций создают durable jobs с устойчивым ID. Локальный оркестратор входов готовит последовательность срезов из хешированного плана и повторяет сохранённые входы после рестарта. Первичный вход сохраняет контекст диспетчера в той же транзакции. Go сохраняет наблюдение явно заданного календарного окна, если оно закрылось не более 24 часов назад. Статус сверяется с сохранённым заданием. Сигналы для детектора событий поступают из подготовленного архива.
- **Goal:** dependable event scheduling без Redis/Kafka.
- **Context:** duplicate triggers не должны создавать двойные trades.
- **Files/components:** Go scheduler поверх jobs/leases/outbox из S2 и research worker из S3.
- **Acceptance:** расписание и события стратегии создают jobs с устойчивым trigger ID, заданным budget и ссылкой на policy/snapshot. Состояние восстанавливается из SQL.
- **Tests:** duplicate catalyst, missed schedule after restart, lease recovery, concurrent trigger delivery.
- **Dependencies:** S2, S3, P2-01, P4-05.

### [ ] P5-02 — Position/thesis review lifecycle

- **Текущий прогресс:** Go восстанавливает срок и триггер из сохранённого research-решения, исполнения и открытых FIFO-лотов. `paper-review-cycles` готовит свежий снимок по каждому заданному окну, записывает точный запрос перед отправкой и повторяет его после сбоя. `schedule-review` сохраняет одну запись на триггер. Реестр и архивный катализатор сверяются с исходной бумагой. Отчёт `0.2` проверяет FIFO-продажи по той же бумаге и оценивает остаток по полной firm-котировке из локальной ленты. Поступление событий из наблюдаемого потока и измеренные затраты выхода остаются открыты.
- **Goal:** автономные holds/adds/reductions по horizon/event.
- **Context:** без forced daily exit и basis anchoring.
- **Files/components:** `services/research/tradingbot/positions.py`, `review.py`.
- **Acceptance:** thesis state отдельно от order state. `next_review_at`, expiry
  и триггеры (отчёт, значимое изменение цены/новости, ухудшение ликвидности,
  риск политики) планируют повторную оценку. Она использует текущий снимок,
  принятые резервы, измеренные затраты выхода и остаток позиции по held mint.
- **Tests:** review event once; partial opening; improved forward thesis despite
  loss; holding after unchanged catalyst; остаток и резервы при отклонении SELL.
- **Dependencies:** P1-05, P5-01.

### [ ] P5-03 — Deterministic risk and reservations

- **Расходы исполнения:** опциональный `max_execution_cost_bps` в политике владельца ограничивает ухудшение цены относительно закреплённой отметки плюс комиссию. Проверяются пределы резерва и фактический объём частичного исполнения симулятора. Превышение сохраняется как `maximum_execution_cost` до резервирования.
- **Текущий прогресс:** `paper-apply` проверяет policy, версию снимка, точный инструмент, расходы, TTL, достаточность средств и концентрацию. На PostgreSQL проверены исполнение по сохранённому решению, частичные fills и повтор без двойного списания. Отчёт `paper-apply refusals` показывает зафиксированные до резервирования отказы по лимиту риска. [Процедура проверки](../runbooks/paper.md#отказы-проверки-риска-до-резервирования).
- **Goal:** account limits перед simulated execution.
- **Context:** AI decides signal, kernel only safety.
- **Files/components:** `services/control/internal/risk/`, SQL reservation transactions, `config/risk.yaml`.
- **Acceptance:** один Go writer проверяет snapshot/registry version, account mode, exact units, eligibility, открытые резервы, лимиты позиции/совокупной экспозиции, turnover, fee, данные и quote TTL. BUY резервирует имеющийся USDC, SELL — exact held mint, обычные действия проходят по одной policy. При неизвестном статусе сохраняется резерв и действует явно заданное ограничение на новые операции.
- **Tests:** одновременные BUY, stale version, превышение коррелированной экспозиции/fee cap, неизвестный баланс, рассогласование с лимитом инструмента или quote expiry, повтор события, no leverage.
- **Dependencies:** P2-01, P1-02, P3-03.

### [ ] P5-04 — Forward paper venue and ledger reconciliation

- **Вход по устойчивому ID:** план `0.2` и лента `0.3` фиксируют
  бумагу, инструмент, реестр и время наблюдения. Диспетчер сверяет
  точные байты ленты перед резервированием и восстанавливает принятую
  заявку по сохранённым байтам.
- **Локальная лента:** профиль `paper-quote` подбирает синтетические
  наблюдения из хешированной ленты счёта и закрепляет их для завершённых
  заданий. При смене политики диспетчер продолжает ранее записанное
  применение по исходной квитанции и котировке.
- **Текущий прогресс:** paper ledger атомарно сохраняет fill и событие уведомления. После таймаута хешированное локальное наблюдение связывается с исходной заявкой и котировкой. Проверены частичная сверка, снятие оставшегося резерва и точный повтор после перезапуска. Relay и consumer проверены с PostgreSQL, локальным Kafka и подставным приёмником. Для forward оценки нужны наблюдавшиеся рыночные котировки и согласованный ряд NAV с полными расходами.
- **Goal:** prospective feasible fills, not rating-only results.
- **Context:** quotes recorded after decision completion.
- **Files/components:** `services/control/internal/venues/paper/`, `services/evaluation/forward.py`.
- **Acceptance:** отдельный paper account. Симулятор принимает только свежую наблюдаемую котировку после решения, явно помечает simulated fill и сохраняет reject/timeout. Ledger сравнивает расчётный и реализованный в симуляции output, публикует NAV включая idle capital и OPEX и синхронный benchmark. Forward run начинается после freeze.
- **Notifications:** интегрировать отдельный сервис уведомлений с подтверждёнными paper fills, сохранив транзакционную запись outbox, стабильный ID исполнения и отсутствие события после rollback.
- **Tests:** no stale/soft-as-guaranteed fill; outage retained; amount conservation, next-fill timestamp.
- **Dependencies:** P5-02, P5-03, P3-04.

### [ ] P5-05 — Monitoring, backup and recovery

- **Журнал входов:** проверка восстанавливает PostgreSQL и отдельный том
  журнала. Она различает задание, уже принятое базой, и запрос, который
  планировщик записал перед сбоем. Оба ID сохраняются при повторе.
- **Календарь:** `research-schedule-coverage` сверяет каждое окно неизменённого расписания с журналом PostgreSQL. Отчёт различает подтверждённый пропуск и отсутствие сведений, показывает время приёма и завершения заданий, попытки и ответы на разрешённые вызовы.
- **Текущий прогресс:** восстановление логического архива PostgreSQL проверено на чистом экземпляре. Сохранились резервы, история сделок, завершённые jobs и выданные разрешения. Прерванное задание продолжилось после истечения lease. `paper-check --pipeline` в одном снимке показывает задержки очереди, аренды, применения, подтверждения и публикации исполнений. [Процедура проверки](../runbooks/paper.md).
- **Goal:** наблюдаемая эксплуатация и восстановление.
- **Context:** paper выявляет operational problems до funds.
- **Files/components:** `services/research/tradingbot/observability.py`, `services/control/internal/telemetry/`, `docs/runbooks/paper.md`, `deploy/`.
- **Acceptance:** trace chain, budgets/reconciliation/stale alerts, backups, successful restore and secret redaction; EU data-processing check recorded.
- **Tests:** injected feed outage/NAV mismatch/spend spike; restored run resumes once; no credentials in logs.
- **Dependencies:** P5-04.

### [ ] P5-06 — Forward evidence and power review

- **Покрытие состояний индекса:** опциональный `regime_review` использует предшествующее изменение индексных уровней, известное до начала каждой пары доходностей. Отчёт сохраняет восходящие, нисходящие, боковые и неклассифицированные пары, учитывает повторные уровни по времени наблюдения и показывает базу расходов. Реальная выборка по этим группам ещё не проверена.
- **Проверка истории:** `paper-nav --verify-report` воспроизводит сохранённый отчёт из PostgreSQL. В режиме `require_prior_receipt` каждая контрольная дата сверяет предшествующую квитанцию и сохраняет строки NAV, индекса и расходов за старые сутки. Локальный архив индекса поддерживается с заявленным статусом доступа.
- **Текущий прогресс:** `forward-paper-report` принимает заранее фиксируемые счёт, капитал, источник цен, набор контрольных дат, эталон и порог просадки. На каждой контрольной дате он сверяет отдельные хеши полного дневного NAV, индекса и расходов. Доступны свободный USD и синтетический индекс полной доходности. При неизвестных расходах виден только execution-net. `forward-paper-power` до появления NAV воспроизводит условную оценку чувствительности основного сравнения на зарегистрированном финальном горизонте. Отчёт сохраняет допущения AR(1), вероятность обнаружения эффекта, долю ложных пересечений порога и ошибку симуляции. Полная forward-выборка, проверка допущений на рыночных данных, покрытие рыночных режимов и внешнее подтверждение времени регистрации остаются открытыми.
- Условную чувствительность блочного интервала можно сопоставить с опциональным нормальным интервалом среднего по заранее фиксированной ковариации AR(1). Аналитические вероятности проверяются независимыми симуляциями. Условный результат строится лишь на финальном чекпойнте в выбранной базе NAV и сохраняется рядом с эмпирическим блочным интервалом.
- **Goal:** принять go/hold/stop по пререгистрированным данным.
- **Context:** 8–12 weeks reliability не автоматически alpha proof.
- **Files/components:** `experiments/forward/reports/`, `docs/gates/G5.md`.
- **Acceptance:** monthly frozen gross/execution-net/operating-net metrics, block CI/power/regime coverage, no dropped days; explicit adequate/inconclusive/negative outcome.
- **Tests:** recompute from ledger; benchmark/cost invariants; decision made without test retuning.
- **Dependencies:** P5-05, observed prospective sample.

### [x] P5-07 — Аналитика портфеля в Telegram

- **Текущий прогресс** — Go-сервис хранит проекцию Kafka в отдельном PostgreSQL и отдаёт команды `/portfolio`, `/positions` и `/performance`. Дневная доходность, просадка, сравнение с эталоном и результат после дополнительных расходов рассчитываются по доступным входам. Рынок, эталон и расходы владельца требуют настройки архивов. [ADR-003](../decisions/003-portfolio-analytics.md).
- **Acceptance:** точные cash, резервы, позиции, NAV и P&L со временем и статусом полноты. Повтор Kafka и восстановление дают тот же результат, бот раскрывает данные только владельцу.
- **Проверки** — [сквозной paper-прогон](../verification.md#аналитика-портфеля) и локальный HTTP-стенд Telegram.
- **Dependencies:** события paper ledger из P5-04, архив оценок и benchmark. `P5-06` использует свои замороженные входы и отчёты для оценки стратегии.

## Phase 6 — Execution security

### [ ] P6-01 — One permitted venue build/submit/status adapter

- **Goal:** minimal execution path без universal router.
- **Context:** start with one approved chain/venue and real versioned docs.
- **Files/components:** `services/control/internal/venues/jupiter/` либо иной approved adapter, `services/control/cmd/executor/`.
- **Acceptance:** один разрешённый путь Jupiter order/execute с проверенными request ID, сроком котировки и blockhash validity, связь `intent_id` → quote/order → message hash → подпись → статус. Хеш связывает артефакты и policy, подписывается точное сериализованное сообщение Solana. Контракт записывает нужные signer slots и семантику внешней отправки. Новый message после timeout возможен после сверки и отдельного решения Go.
- **Tests:** API recorded contract, expired quote, unknown submission status, minimum/fee error, неверная или неполная подпись, изменённое сообщение, отказ reserve до submit.
- **Dependencies:** P3-01, P5-03, G0/G3 access.

### [ ] P6-02 — Independent transaction decoder/policy verifier

- **Goal:** reject malicious built transaction до signing.
- **Context:** quote provider untrusted; Token-2022/lookup tables critical.
- **Files/components:** `services/control/internal/verify/`, adversarial transaction fixtures.
- **Acceptance:** full resolved account/program/instruction decode, exact source/destination/mints/min-output/fees; unknown/delegate/authority/bridge rejected.
- **Tests:** hidden transfer, fake ATA, changed recipient, unknown program, lookup-table mutation, token extension, fee drain.
- **Dependencies:** P6-01, P0-04.

### [ ] P6-03 — Pure Ed25519/KMS compatibility spike

- **Goal:** подтвердить concrete signing algorithm/region.
- **Context:** KMS availability не trading policy.
- **Files/components:** `signer/spike/`, `docs/adr/signer.md`.
- **Acceptance:** known-vector Solana message verification, RAW pure Ed25519 algorithm, selected EU region/provider access, latency/cost and recovery constraints.
- **Tests:** wrong prehash variant fails; signature verifies with maintained SDK; no private key export/log.
- **Dependencies:** P0-02, P6-02.

### [ ] P6-04 — Isolated signer and signed risk config

- **Goal:** execution host не имеет unrestricted signing.
- **Context:** verifier independent trust boundary.
- **Files/components:** `signer/service/`, `signer/policy/`, `deploy/signer/`.
- **Acceptance:** authenticated requests, policy/intent/transaction hash verification, nonce/expiry/caps, owner config, separate IAM role; Python/Go caller no KMS Sign.
- **Tests:** unauthorized caller, arbitrary payload, stale config, replay, cap bypass, compromised executor scenario.
- **Dependencies:** P6-02, P6-03, threat-model review.

### [ ] P6-05 — Submit idempotency and finality reconciliation

- **Goal:** exactly-once economic effect where chain semantics allow.
- **Context:** API timeout ≠ failed transaction.
- **Files/components:** `services/control/internal/orders/`, `services/control/internal/reconcile/`, SQL order events.
- **Acceptance:** submitted/unknown/confirmed/finalized/reconciled states; independent RPC balances; before resend status and expiry checks, reserved balances remain conservative.
- **Tests:** timeout then late finalization, node disagreement, duplicate callback, expired blockhash/resubmit, partial portfolio completion.
- **Dependencies:** P6-01, P5-03.

### [ ] P6-06 — Cold/float refill and emergency recovery runbook

- **Goal:** funds outside bot remain outside compromise radius.
- **Context:** raw-key full compromise can drain float.
- **Files/components:** `docs/runbooks/wallet.md`, `docs/runbooks/incident.md`.
- **Acceptance:** пополнение и вывод выполняет владелец по проверенным адресам. Cold treasury отделена от торгового кошелька и недоступна signer. Документированы допустимый on-/off-ramp, ручной перевод USDC и SOL под gas/ATA, лимиты total/float/gas, остановка новых подписей вне application host, сверка pending-транзакций и план возврата остатка. Для non-exportable key описаны восстановление IAM/доступа к ключу и аварийные действия при его недоступности.
- **Tests:** tabletop и sandbox restore, bot не может инициировать refill/вывод, остановка и статус уже отправленной транзакции проверены отдельно, восстановленные chain balances согласуются с ledger.
- **Dependencies:** P6-04, P6-05.

### [ ] P6-07 — Adversarial review and gate report

- **Goal:** complete safety evidence before mainnet funds.
- **Context:** good backtest cannot waive signing risk.
- **Files/components:** `docs/gates/G6.md`, security corpus/tests, dependency manifest.
- **Acceptance:** all critical TM paths tested; no unresolved arbitrary signing/double-spend/accounting issue; terms refreshed; independent verifier boundary substantiated.
- **Tests:** prompt injection→intent→signer chain, compromised quote builder, unauthorized IAM, dependency/restore scenario.
- **Dependencies:** P6-02…06, P5-05.

### [ ] P6-08 — Optional second venue adapter behind capability gate

- **Goal:** add xChange/Ondo only when measurable benefit justified.
- **Context:** not prerequisite for one-path pilot; no speculative API implementation.
- **Files/components:** `services/control/internal/venues/xchange/` или `ondo/`, generated clients/fixtures.
- **Acceptance:** approved access/minimum/settlement asset; normalized quotes, decoded transaction policy and reconciliation; if unavailable ticket deferred without blocking pilot.
- **Tests:** hard/soft, attestation expiry, issuer limits/halts, USDon residual balance, held-wrapper exit.
- **Dependencies:** own P0-01/P3 evidence, P6-07.

## Phase 7 — Bounded live

### [ ] P7-01 — Approve risk envelope and fund micro canary

- **Goal:** owner-controlled first mainnet validation.
- **Context:** planned cap/minimum must agree; no human approval per ordinary trade afterward.
- **Files/components:** `docs/gates/G7-start.md`, `config/live-envelope.yaml`, owner wallet action outside code.
- **Acceptance:** основные gates доступа/стоимости/paper/безопасности пройдены. Владелец подписывает версию политики с total/float/SOL gas reserve, лимитами позиции/ордера/оборота/убытка/просадки, минимальным доступным размером, max fee/slippage, допустимой сессией и условиями stop/resume. `research`/`paper` используют отдельные credentials и не могут активировать `live`. Первое пополнение и активация явно подтверждены владельцем с audit evidence без секретов.
- **Tests:** allowed payload/exit checked, killswitch responsive, pre/post balances match.
- **Dependencies:** P5-06 passing gate, P6-07; P6-08 only for that chosen route.

### [ ] P7-02 — Canary settlement and controlled exit report

- **Goal:** проверить real path и return-to-cold ability.
- **Context:** testnet не подтверждает mainnet issuer liquidity.
- **Files/components:** `experiments/live-canary/report.md`, live order/fee ledger.
- **Acceptance:** ограниченный round trip USDC→один допущенный xStock→USDC с проверенным exact mint, отдельной котировкой на выход, независимой finality сверкой и фактическими delta balances/fees. Восстановление сверяет разрешённый перевод остатка в cold treasury. Несовпадение или неопределённое исполнение останавливает новые операции до разбирательства.
- **Tests:** independent RPC and owner wallet balances, actual bps vs quote, no duplicate economic effect.
- **Dependencies:** P7-01.

### [ ] P7-03 — Bounded $1k total-capital pilot operation

- **Goal:** autonomous operation внутри small float envelope.
- **Context:** $1 000 общий капитал и $250–500 float — предварительные сценарии. Фактические размеры определяются в P7-01 по доступным котировкам и потере, которую владелец готов принять. NAV включает idle capital в cold treasury.
- **Files/components:** `experiments/live-pilot/`, operational config and reports.
- **Acceptance:** frozen strategy, actual costs/OPEX, incidents/rejections retained, no auto-refill/scale, daily marks plus thesis reviews.
- **Tests:** cap/reduce-only/freeze events; reconciliation and spend monitors; replay from audit bundle.
- **Dependencies:** P7-02.

### [ ] P7-04 — Live-vs-paper and stop/continue review

- **Goal:** отделить implementation slippage от alpha claim.
- **Context:** few trades не statistical success.
- **Files/components:** `docs/gates/G7-review.md`, live comparative reports.
- **Acceptance:** fill/quote drift, net excess CI/power, fee distribution, drawdown, incident root causes; next action stop/paper/continue clearly recorded.
- **Tests:** recomputed NAV including cold idle reserve; no excluded failed tx or monthly cost.
- **Dependencies:** P7-03 and adequate observed sample.

## Phase 8 — Capacity/termination

### [ ] P8-01 — Capital and capacity cost curves

- **Goal:** проверить next-size economics без leverage.
- **Context:** scaling may dilute OPEX but worsen market impact.
- **Files/components:** `experiments/capacity/`, quote-size report.
- **Acceptance:** tested size buckets, all-in cost/availability/issuer concentration, capital breakeven only with measured excess uncertainty; no promised return.
- **Tests:** non-linear slippage, maximum venue limits, liquidity outage, cash reserve requirements.
- **Dependencies:** P7-04, refreshed P3 study.

### [ ] P8-02 — Independent evidence/security review

- **Goal:** проверить scale case вне разработки.
- **Context:** author self-report недостаточен.
- **Files/components:** `docs/reviews/scale.md`, evidence bundle.
- **Acceptance:** reviewer can reconstruct results/fees/terms/controls; unresolved high-risk findings block scale; licenses and provider processing regions refreshed.
- **Tests:** independent rerun accounting/statistical reports; boundary and recovery review at new cap.
- **Dependencies:** P8-01.

### [ ] P8-03 — Scale, simplify or terminate decision

- **Goal:** выполнить objective-oriented итог, не спасать исходную гипотезу.
- **Context:** no net alpha → strategy not successful.
- **Files/components:** `docs/gates/G8.md`, archived run manifest, owner next-cap config при GO.
- **Acceptance:** решение и подтверждающие данные явные. Увеличение лимитов
  требует достаточных положительных результатов с учётом всех затрат, риска
  и capacity. Иначе варианты включают продолжение paper, остановку и возврат
  остатка или упрощение стратегии. Смена DeFi на брокера оформляется отдельным
  решением владельца.
- **Tests:** terminal run immutable; returns/recovery reconciled; next config can't silently change previous evidence.
- **Dependencies:** P8-02.

## Checklist состояния

- [x] Прочитан запрос, принят EU основной сценарий.
- [x] Проверены первичные документы и pinned upstream source snapshots.
- [x] Составлены 37-section architecture/research plan и малые tickets.
- [x] Unknown quotes, legal eligibility и alpha отделены от доказанных фактов.
- [ ] P0–P8 implementation/experiments выполнены — **нет, требуют следующих этапов**.
- [ ] Автономный live допущен — **нет**.
- [ ] Net risk-adjusted alpha доказана — **нет**.
