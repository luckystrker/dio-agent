-- 17.5 (2026-11-02): уникальность dedupe_key — только на живые намерения.
-- Прежний вечный UNIQUE держал ключ и после терминального статуса: завершённое
-- или отменённое намерение навсегда блокировало такое же — reply_later с темой
-- = последний текст пользователя молча терял повторную просьбу теми же словами,
-- инициативы по темам выгорали со временем. Частичный индекс охраняет дубль
-- только пока намерение живо (pending/running, §14.1).
ALTER TABLE intentions DROP CONSTRAINT intentions_dedupe_key_key;
CREATE UNIQUE INDEX intentions_dedupe_live_uidx ON intentions (dedupe_key) WHERE status IN ('pending', 'running');
