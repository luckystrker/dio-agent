-- Терминальный провал хода (29.1, §11.3): после исчерпания попыток ход не
-- остаётся в промежуточном статусе и не «воскресает» recovery — строка
-- получает status='failed' с причиной. Причина дублирует событие turn_failed
-- в журнале хода для оператора: SQL-запрос «почему ход встал» не требует
-- чтения event_streams.
ALTER TABLE turns
    ADD COLUMN failure_reason text;

COMMENT ON COLUMN turns.failure_reason IS 'причина терминального провала (status=failed, 29.1); у остальных статусов — NULL';
COMMENT ON COLUMN turns.status IS 'created | applied | failed (терминальный провал 29.1) | skipped (терминальный пропуск)';
