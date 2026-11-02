// Ходы-сироты инициатив (блок 18.7): ход создаётся до генерации (status
// created); если намерение стало терминальным любым путём, минующим отмену с
// пометкой (крах между созданием хода и гейтом, ручные правки), ход навсегда
// остаётся created без событий — worker его не берёт (нет событий), очередь
// §20 замусоривается. Страховка в такте помечает такие ходы skipped.
package background

import (
	"context"
	"fmt"
)

// skipOrphanInitiativeTurns — страховка такта: created-ходы без событий, чьё
// намерение терминально (cancelled/failed/completed), помечаются skipped с
// записью turn_skipped в журнал. Живые ходы (прямые обращения с событиями,
// created-ходы живого намерения) не трогаются.
func (s *Scheduler) skipOrphanInitiativeTurns(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id FROM turns t
		WHERE t.status = 'created'
		  AND NOT EXISTS (SELECT 1 FROM social_events e WHERE e.turn_id = t.id)
		  AND EXISTS (
		    SELECT 1 FROM intentions i
		    WHERE i.related_turn_id = t.id
		      AND i.status IN ('cancelled', 'failed', 'completed'))`)
	if err != nil {
		return fmt.Errorf("фон: ходы-сироты не читаются: %w", err)
	}
	defer rows.Close()

	var orphans []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("фон: строка хода-сироты не читается: %w", err)
		}
		orphans = append(orphans, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("фон: ходы-сироты не дочитаны: %w", err)
	}

	for _, id := range orphans {
		s.skipTurn(ctx, id, "intention", "намерение терминально, ход без событий (блок 18.7)")
	}
	if len(orphans) > 0 {
		s.logInfo("фон: страховка пометила skipped ходов-сирот: %d (блок 18.7)", len(orphans))
	}
	return nil
}
