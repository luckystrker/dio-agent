-- 17.14 (2026-11-02): частичный индекс выборки необработанных событий.
-- Такт планировщика каждые 15–30 с читает social_events WHERE
-- status='processed' AND summarized_at IS NULL — без индекса это полный
-- скан, стоимость такта растёт с историей. Частичный индекс под предикат и
-- порядок выборки (chat_id, occurred_at, id) держит только необработанный
-- хвост: размер пропорционален очереди консолидации, а не таблице.
CREATE INDEX social_events_pending_episodes_idx
  ON social_events (chat_id, occurred_at, id)
  WHERE status = 'processed' AND summarized_at IS NULL;
