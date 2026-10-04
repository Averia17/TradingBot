# Clean upstream verification — 2026-10-04

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
