-- 16.9 (2026-11-02): частичный индекс очереди ходов. Когнитивный worker
-- опрашивает turns WHERE status='created' каждые 500 мс (§5.2); индекса по
-- status не было (только PK и chat_id,created_at) — каждый опрос сканировал
-- растущую таблицу целиком. Частичный индекс держит только созданные ходы:
-- размер пропорционален длине очереди, а не истории.
CREATE INDEX turns_created_partial_idx ON turns (status) WHERE status = 'created';
