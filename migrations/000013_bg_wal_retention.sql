-- Ретенция WAL фоновых задач (блок 18.6): event_streams — append-only журнал
-- (§12.1, инвариант 13), но WAL-потоки брошенных фоновых задач (bg:*) без
-- живой задачи и старше 30 дней — мёртвый след: у растущего эпизода меняется
-- lastEventID → новый unitID, старый поток уже никогда не читается.
-- Триггер сохраняет механику «изменение и удаление запрещены», добавляя
-- ЕДИНСТВЕННОЕ исключение: удаление строк bg-потоков старше 30 дней.
-- Условие «без живой задачи» накладывает сама чистка (internal/background/
-- retention.go) в WHERE — триггер гарантирует только возраст и принадлежность
-- фону, поэтому свежие и чужие потоки не удаляются ничем и никогда.

DROP TRIGGER IF EXISTS event_streams_append_only ON event_streams;
DROP FUNCTION IF EXISTS forbid_event_streams_mutation();

CREATE FUNCTION forbid_event_streams_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.stream_id LIKE 'bg:%'
       AND OLD.created_at < now() - interval '30 days' THEN
        RETURN OLD; -- ретенционный вывоз мёртвых WAL-потоков фона (блок 18.6)
    END IF;
    RAISE EXCEPTION 'event_streams — append-only журнал (§12.1): % запрещено', TG_OP;
END;
$$;

CREATE TRIGGER event_streams_append_only
    BEFORE UPDATE OR DELETE ON event_streams
    FOR EACH ROW EXECUTE FUNCTION forbid_event_streams_mutation();
