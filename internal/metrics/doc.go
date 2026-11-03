// Пакет metrics — эндпоинт наблюдаемости §20 (этап 31).
//
// HTTP слушает только 127.0.0.1 на порту METRICS_PORT (пусто — выключен):
// /metrics — Prometheus text exposition, /stats — читаемый JSON, /healthz —
// проверка живости. Метрики считаются SQL-запросами по таблицам-источникам
// истины (turns, event_streams, delivery_outbox, intentions, budget_counters,
// character_state, stickers, memory_items) с кэшем на 15 секунд, поэтому
// переживают рестарт процесса и не расходятся с журналом (философия №2).
// Счётчики в памяти описывают только сам сервер метрик (ошибки скрейпа,
// возраст кэша, аптайм) — им в базе места нет.
package metrics
