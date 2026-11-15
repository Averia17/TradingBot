# ADR-002 — Уведомления об исполненных сделках

Принято 15.11.2026. Владелец запросил отдельный Go-сервис уведомлений,
Kafka и транзакционный outbox на pgqueue.

`services/trade-events/` владеет HTTP и pgqueue outbox.
`services/notifications/` владеет Kafka consumer и Telegram. У каждого сервиса
свои Go-модуль, реализация контракта, Dockerfile и образ. Совместимость задаёт
версионированная JSON Schema и проверяют общие fixtures из `contracts/`.
Сервисы обмениваются событиями через Kafka.
В [Compose](../../deploy/notifications/compose.yaml) профиль `test` включает
`POST /v1/test/exchange/fills` на loopback хоста. Endpoint принимает paper fill
и присваивает ему источник `exchange_fixture`. `trade-events` сохраняет
тестовую запись и очередь в одной транзакции. Ошибка enqueue приводит к
rollback. Повтор того же ID и содержимого сохраняет прежнюю запись, изменение
содержимого даёт конфликт. Каждый partial fill получает отдельный стабильный ID.
Интеграция с авторитетной транзакцией ledger относится к S5. Research jobs
используют свой PostgreSQL outbox.

Relay внутри `trade-events` обрабатывает очередь `trade-filled-v1` через
`github.com/goware/pgqueue`. PostgreSQL освобождает транзакцию claim перед
сетевым вызовом. Relay завершает задачу после Kafka ACK от in-sync replicas.
Сообщение содержит [fill JSON версии 1.0](../../contracts/trade-event.schema.json)
с `id` как Kafka key. Денежные значения передаются десятичными строками. События
`exchange_fixture` публикуются в выделенный topic `tradingbot.trade.filled.fixture.v1`.

Контейнер `notifications` читает topic своей consumer group и сохраняет
offset после ответа Telegram об успешной отправке в настроенный личный чат.
Telegram принимает числовой chat ID после `/start`. Compose передаёт токен и ID
только в `notifications` через secrets. Процесс получает их из своего окружения.
Уведомления о симуляции помечаются `PAPER · ТЕСТ`.

Гарантия доставки соответствует at-least-once. Повтор возможен при сбое между
Kafka ACK и завершением outbox task или между отправкой в Telegram и фиксацией
offset. Telegram `sendMessage` не принимает idempotency key, поэтому fill ID
присутствует в сообщении для сверки. При невалидной записи или ошибке доставки
consumer останавливается с указанием позиции Kafka, сохраняя offset для
разбора и повтора. При явном HTTP 429 consumer может подождать `retry_after`
до десяти секунд и повторить один раз.

После исчерпания 20 попыток публикации pgqueue хранит задачу как `failed`.
После устранения причины оператор может восстановить её через
`trade-events -requeue-task <task_id>`. ID виден в журнале relay. Сохраняем
завершённые строки для дедупликации до появления политики retention.
PostgreSQL и Kafka доступны внутри выделенных сетей Compose. На хосте доступен
только тестовый HTTP endpoint. Срок хранения событий в topic должен покрывать
возможный простой consumer.

Отдельный consumer хранит только своё положение в Kafka и не получает доступ
к ledger или исполнению. Telegram token нужен только этому процессу.
Транзакционный outbox сохраняет событие при недоступности Kafka. Прямая
публикация из writer оставила бы окно потери между записью fill и отправкой.

Локальный запуск на Docker Desktop 15.11.2026 подтвердил HTTP `201` для нового
тестового fill, `200` для повтора, `409` для изменения под тем же ID и `422`
для `live`. В PostgreSQL остались одна запись fill и одна завершённая задача
outbox. Telegram подтвердил одну отправку, Kafka зафиксировала offset с lag `0`.
