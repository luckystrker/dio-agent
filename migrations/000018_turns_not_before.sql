-- Отложенный ход по дневному бюджету (29.2): ход, упёршийся в суточный лимит
-- модельных вызовов, ждёт сброса счётчика до следующего дня 00:05 UTC — срок
-- живёт в строке хода, а не только в памяти процесса. Рестарт больше не делает
-- такой ход доступным раньше времени (прежнее notBefore терялось), очередь
-- (worker.claimNext) уважает not_before.
ALTER TABLE turns
    ADD COLUMN not_before timestamptz;

COMMENT ON COLUMN turns.not_before IS 'не раньше этого срока брать created-ход (29.2: отложен до сброса дневного бюджета); NULL — без отсрочки';

-- Ход с отсрочкой не должен «застревать» в очереди незаметно: частичный
-- индекс очереди по сроку помогает выборке claimNext.
CREATE INDEX turns_created_not_before_idx ON turns (not_before)
    WHERE status = 'created';
