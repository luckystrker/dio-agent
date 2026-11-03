// Наблюдаемость фона (§20): каждый фоновый ход логируется ОДНОЙ записью —
// IDs, decision, outcome, latency, usage, бюджет и основания изменений
// настроения; полный промпт не логируется. queueStats с предупреждением на
// рост очереди прямых обращений; heartbeat — на уровне debug (каждый такт
// не должен заглушать полезные строки лога).
package background

import (
	"context"
	"fmt"
	"time"
)

// QueueStats — снимок очереди работы (§20: queue wait и возраст старейшего хода).
type QueueStats struct {
	// PendingTurns — ходы прямых обращений, ждущие когнитивного worker
	// (created с привязанными событиями).
	PendingTurns int
	// OldestTurnAgeSec — возраст старейшего такого хода, секунд.
	OldestTurnAgeSec float64
	// PendingOutbox — доставки, ждущие отправки или повторной попытки.
	PendingOutbox int
	// DueIntentions — намерения с наступившим сроком (ещё не забранные).
	DueIntentions int
	// Boredom — текущая шкала скуки 0..1 (этап 23.4): видна в heartbeat.
	Boredom float64
}

// QueueStats читает снимок очереди (диагностика §20; вызовов модели нет).
func (s *Scheduler) QueueStats(ctx context.Context) (QueueStats, error) {
	var stats QueueStats
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(max(extract(epoch FROM (now() - t.created_at))), 0)
		FROM turns t
		WHERE t.status = 'created'
		  AND EXISTS (SELECT 1 FROM social_events e WHERE e.turn_id = t.id)`).
		Scan(&stats.PendingTurns, &stats.OldestTurnAgeSec)
	if err != nil {
		return QueueStats{}, fmt.Errorf("фон: очередь ходов не читается: %w", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE status IN ('pending', 'retry_wait')`).
		Scan(&stats.PendingOutbox); err != nil {
		return QueueStats{}, fmt.Errorf("фон: очередь доставки не читается: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM intentions
		WHERE status = 'pending' AND not_before <= now() AND expires_at > now()`).
		Scan(&stats.DueIntentions); err != nil {
		return QueueStats{}, fmt.Errorf("фон: очередь намерений не читается: %w", err)
	}
	// Скука (этап 23.4): heartbeat показывает, насколько персонажу пусто.
	// Сбой чтения не валит такт: скука — необязательная активность, а стоп
	// всего фона из-за диагностической метрики недопустим.
	if boredom, berr := s.boredomNow(ctx); berr != nil {
		s.logWarn("heartbeat: скука не прочитана: %v", berr)
	} else {
		stats.Boredom = boredom
	}
	return stats, nil
}

// growthWarnTicks — после скольких тактов непрерывного роста очередь считается
// растущей и оператор получает предупреждение (§20: «рост очереди требует
// предупреждения оператора через локальные логи/метрики»).
const growthWarnTicks = 3

// observeTick — heartbeat (debug) и предупреждение на рост очереди прямых
// обращений. Предупреждение — один раз на эпизод роста; сокращение очереди
// снимает и счётчик, и пометку.
func (s *Scheduler) observeTick(stats QueueStats) {
	s.logDebug("heartbeat: фон жив: ходов=%d oldest=%.0fs outbox=%d due_intentions=%d boredom=%.2f",
		stats.PendingTurns, stats.OldestTurnAgeSec, stats.PendingOutbox, stats.DueIntentions, stats.Boredom)

	s.mu.Lock()
	defer s.mu.Unlock()
	if stats.PendingTurns > s.lastPending && stats.PendingTurns > 0 {
		s.growthStreak++
		if s.growthStreak >= growthWarnTicks && !s.growthWarned {
			s.logWarn("рост очереди прямых обращений: %d ходов (было %d тактов подряд растёт), фон откладывается — проверь когнитивный worker (§20)",
				stats.PendingTurns, s.growthStreak)
			s.growthWarned = true
		}
	} else {
		s.growthStreak = 0
		s.growthWarned = false
	}
	s.lastPending = stats.PendingTurns
}

// TaskRecord — одна запись лога о фоновом ходе (§20). Основания mood — из
// применённой дельты (инициатива применяет mood_delta через общий путь
// применения хода); основания отношений в фоне не применяются (у инициативы
// нет актора пачки) и поле остаётся пустым — пустота честная.
type TaskRecord struct {
	Unit        string // id задачи = stream WAL
	Kind        string // initiative | explore | consolidate
	IntentionID int64
	ChatID      int64
	Decision    string
	Outcome     string // completed | cancelled | failed | deferred | noop | error
	Latency     time.Duration
	Stats       modelStats
	BudgetOp    string
	MoodReason  string
	RelReason   string
	Detail      string
	// Origin — причина explore (этап 23.4): boredom | conversation.
	Origin string
	// Boredom — шкала скуки на момент задачи (этап 23.4).
	Boredom float64
}

// recordIntention пишет одну запись §20 об исполненном намерении.
func (s *Scheduler) recordIntention(it Intention, out intentionOutcome) {
	rec := TaskRecord{
		Unit:        intentionUnitID(it.ID),
		Kind:        it.Kind,
		IntentionID: it.ID,
		ChatID:      it.TargetChatID,
		Decision:    out.Decision,
		Outcome:     out.Outcome,
		Latency:     out.Latency,
		Stats:       out.Stats,
		BudgetOp:    out.BudgetOp,
		MoodReason:  out.MoodReason,
		Detail:      out.Detail,
		Origin:      out.Origin,
		Boredom:     out.Boredom,
	}
	s.writeRecord(rec)
}

// recordConsolidation пишет одну запись §20 о консолидации эпизода.
func (s *Scheduler) recordConsolidation(ep Episode, out consolidationOutcome) {
	rec := TaskRecord{
		Unit:     consolidationUnitID(ep.ChatID, ep.LastEventID),
		Kind:     "consolidate",
		ChatID:   ep.ChatID,
		Decision: "consolidate",
		Outcome:  out.Outcome,
		Latency:  out.Latency,
		Stats:    out.Stats,
		Detail:   out.Detail,
	}
	s.writeRecord(rec)
}

func (s *Scheduler) writeRecord(rec TaskRecord) {
	s.logInfo("фон: задача=%s kind=%s intention=%d chat=%d decision=%s outcome=%s latency_ms=%d llm_calls=%d tokens=%d budget=%s mood_reason=%q rel_reason=%q origin=%q boredom=%.2f detail=%q",
		rec.Unit, rec.Kind, rec.IntentionID, rec.ChatID, rec.Decision, rec.Outcome,
		rec.Latency.Milliseconds(), rec.Stats.Calls, rec.Stats.Tokens, rec.BudgetOp,
		rec.MoodReason, rec.RelReason, rec.Origin, rec.Boredom, rec.Detail)
}

func (s *Scheduler) logDebug(format string, args ...any) { s.write("DEBUG ", format, args...) }
func (s *Scheduler) logInfo(format string, args ...any)  { s.write("INFO  ", format, args...) }
func (s *Scheduler) logWarn(format string, args ...any)  { s.write("WARN  ", format, args...) }
func (s *Scheduler) logError(format string, args ...any) { s.write("ERROR ", format, args...) }

func (s *Scheduler) write(prefix, format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(prefix+"[background] "+format, args...)
	}
}
