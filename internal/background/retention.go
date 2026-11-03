// Ретенция фона (блок 18.6): суточная чистка в такте планировщика —
// истёкшие cooldown-ключи консолидации (runtime_state), чекпоинты терминальных
// задач старше 7 дней (task_checkpoints), WAL-потоки bg:* без живой задачи
// старше 30 дней (event_streams; исключение в append-only триггере — миграция
// 000013). Это гигиена, а не модельная работа: ошибка чистки не роняет такт.
package background

import (
	"context"
	"time"
)

// Границы ретенции (блок 18.6).
const (
	// checkpointRetention — чекпоинты терминальных задач хранятся 7 дней.
	checkpointRetention = 7 * 24 * time.Hour
	// walRetention — мёртвые WAL-потоки фона вывозятся после 30 дней
	// (согласована с исключением в append-only триггере, миграция 000013).
	walRetention = 30 * 24 * time.Hour
	// retentionSweepInterval — чистка выполняется раз в сутки.
	retentionSweepInterval = 24 * time.Hour
)

// sweepRetention — разовая суточная чистка (сторож lastRetentionSweep, как у
// сверки резервов §16). Часы читаются ДО захвата мьютекса: паника poisoned-
// зависимости не должна оставлять замок занятым (изоляция паник 16.2).
func (s *Scheduler) sweepRetention(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	due := s.lastRetentionSweep.IsZero() || !now.Before(s.lastRetentionSweep.Add(retentionSweepInterval))
	if due {
		s.lastRetentionSweep = now
	}
	s.mu.Unlock()
	if !due {
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logError("фон: чистка ретенции не открыта: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Cooldown-ключи консолидации: формат значения {"until": RFC3339} срока
	// содержит, отдельной колонки не требуется. Битая запись (без until)
	// консолидацию не блокирует (consolidationCooled её игнорирует) — тоже вывоз.
	cooldowns, err := tx.Exec(ctx, `
		DELETE FROM runtime_state
		WHERE key LIKE 'bg:consolidate:cooldown:%'
		  AND (
		    value->>'until' IS NULL
		    OR (value->>'until')::timestamptz <= now()
		  )`)
	if err != nil {
		s.logError("фон: чистка cooldown-ключей не выполнена: %v", err)
		return
	}

	// Чекпоинты терминальных задач: намерение завершено/отменено/провалено,
	// эпизод консолидации сведён. Живая задача (pending/running, несведённый
	// эпизод) чекпоинт не теряет — он точка возобновления (§12.2).
	checkpoints, err := tx.Exec(ctx, `
		DELETE FROM task_checkpoints c
		WHERE c.task_id LIKE 'bg:%'
		  AND c.updated_at < now() - make_interval(secs => $1)
		  AND (
		       (c.task_id ~ '^bg:intention:[0-9]+$' AND EXISTS (
		          SELECT 1 FROM intentions i
		          WHERE i.id = substring(c.task_id FROM '[0-9]+$')::bigint
		            AND i.status IN ('completed', 'cancelled', 'failed')))
		    OR (c.task_id ~ '^bg:consolidate:-?[0-9]+:[0-9]+$' AND EXISTS (
		          SELECT 1 FROM social_events e
		          WHERE e.id = split_part(c.task_id, ':', 4)::bigint
		            AND e.summarized_at IS NOT NULL))
		  )`, checkpointRetention.Seconds())
	if err != nil {
		s.logError("фон: чистка чекпоинтов не выполнена: %v", err)
		return
	}

	// WAL-потоки bg:* без живой задачи: намерение уже не pending/running;
	// у консолидации событие сведено ИЛИ эпизод ушёл вперёд (есть события
	// новее — unitID сменился, этот поток уже никогда не читается). Поток
	// удаляется целиком по возрасту ПОСЛЕДНЕЙ строки (minor 11): иначе
	// выживала голова старого потока, и checkContiguous навсегда получал
	// ErrSequenceGap, если поток когда-нибудь перечитали.
	wal, err := tx.Exec(ctx, `
		DELETE FROM event_streams es
		WHERE es.stream_id LIKE 'bg:%'
		  AND es.stream_id IN (
		      SELECT stream_id FROM event_streams
		      WHERE stream_id LIKE 'bg:%'
		      GROUP BY stream_id
		      HAVING max(created_at) < now() - make_interval(secs => $1)
		  )
		  AND (
		       (es.stream_id ~ '^bg:intention:[0-9]+$' AND NOT EXISTS (
		          SELECT 1 FROM intentions i
		          WHERE i.id = substring(es.stream_id FROM '[0-9]+$')::bigint
		            AND i.status IN ('pending', 'running')))
		    OR (es.stream_id ~ '^bg:consolidate:-?[0-9]+:[0-9]+$' AND NOT EXISTS (
		          SELECT 1 FROM social_events e
		          WHERE e.id = split_part(es.stream_id, ':', 4)::bigint
		            AND e.summarized_at IS NULL
		            AND NOT EXISTS (
		              SELECT 1 FROM social_events newer
		              WHERE newer.chat_id = e.chat_id AND newer.id > e.id)))
		  )`, walRetention.Seconds())
	if err != nil {
		s.logError("фон: чистка WAL-потоков не выполнена: %v", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.logError("фон: чистка ретенции не зафиксирована: %v", err)
		return
	}
	if n := cooldowns.RowsAffected() + checkpoints.RowsAffected() + wal.RowsAffected(); n > 0 {
		s.logInfo("фон: чистка ретенции: cooldown=%d чекпоинтов=%d wal_строк=%d (блок 18.6)",
			cooldowns.RowsAffected(), checkpoints.RowsAffected(), wal.RowsAffected())
	}
}
