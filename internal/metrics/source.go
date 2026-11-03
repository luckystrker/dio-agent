// Сбор снимка метрик из PostgreSQL (этап 31.2): источник истины — база
// (философия №2), поэтому цифры переживают рестарт и не расходятся с журналом.
// Только чтение: затухание настроения и рост скуки вычисляются на лету —
// скрейп мониторинга не должен менять состояние персонажа.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/background"
	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// Source — поставщик снимка. Юнит-тесты сервера подменяют его подделкой.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// SourceOptions — внешние настройки, которые нельзя вывести из базы: лимиты
// конфигурации и цель затухания настроения (персона/расписание из файла).
type SourceOptions struct {
	Baseline func(now time.Time) character.Mood
	Schedule *character.Schedule
	Timezone *time.Location
	Now      func() time.Time

	ConversationCallsLimit int64
	BackgroundCallsLimit   int64
	MaintenanceCallsLimit  int64
}

// PgSource собирает снимок SQL-запросами по таблицам-источникам истины.
type PgSource struct {
	pool *runtime.Pool
	opts SourceOptions
}

// NewPgSource строит источник поверх пула.
func NewPgSource(pool *runtime.Pool, opts SourceOptions) *PgSource {
	return &PgSource{pool: pool, opts: opts}
}

// Snapshot собирает все метрики. Ошибка любого запроса — ошибка снимка: сервер
// отдаст последний удачный кэш, а не частичные нули.
func (s *PgSource) Snapshot(ctx context.Context) (Snapshot, error) {
	now := s.now()
	snap := Snapshot{GeneratedAt: now.UTC()}
	steps := []struct {
		name string
		fn   func(context.Context, *Snapshot, time.Time) error
	}{
		{"решения", s.collectDecisions},
		{"ходы", s.collectTurns},
		{"модель", s.collectModel},
		{"инструменты памяти", s.collectMemoryTools},
		{"первый bubble", s.collectFirstBubble},
		{"очередь", s.collectQueue},
		{"доставки", s.collectDeliveries},
		{"намерения", s.collectIntentions},
		{"состояние персонажа", s.collectCharacter},
		{"стикеры", s.collectStickers},
		{"события жизни", s.collectLifeEvents},
		{"память", s.collectMemory},
	}
	for _, step := range steps {
		if err := step.fn(ctx, &snap, now); err != nil {
			return Snapshot{}, fmt.Errorf("метрики: %s: %w", step.name, err)
		}
	}
	return snap, nil
}

func (s *PgSource) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

func (s *PgSource) collectDecisions(ctx context.Context, snap *Snapshot, _ time.Time) error {
	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(e.payload->'result'->>'decision', ''),
		       count(*) FILTER (WHERE e.created_at >= now() - interval '1 hour'),
		       count(*) FILTER (WHERE e.created_at >= now() - interval '24 hours'),
		       count(*)
		FROM event_streams e
		WHERE e.type = 'model_output'
		  AND e.created_at >= now() - interval '7 days'
		  AND EXISTS (
		    SELECT 1 FROM event_streams s
		    WHERE s.stream_id = e.stream_id
		      AND s.type = 'turn_started'
		      AND s.payload->>'kind' = 'conversation')
		GROUP BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			decision          string
			hour1, day1, day7 int64
		)
		if err := rows.Scan(&decision, &hour1, &day1, &day7); err != nil {
			return err
		}
		counts := DecisionCounts{Hour1: hour1, Day1: day1, Day7: day7}
		switch decision {
		case character.DecisionAnswer:
			snap.Decisions.Answer = counts
		case character.DecisionReact:
			snap.Decisions.React = counts
		case character.DecisionIgnore:
			snap.Decisions.Ignore = counts
		case character.DecisionDefer:
			snap.Decisions.Defer = counts
		}
	}
	return rows.Err()
}

func (s *PgSource) collectTurns(ctx context.Context, snap *Snapshot, _ time.Time) error {
	snap.Turns.ByStatus = make(map[string]int64)
	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*), count(*) FILTER (WHERE created_at >= now() - interval '24 hours')
		FROM turns GROUP BY status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			status             string
			total, createdLast int64
		)
		if err := rows.Scan(&status, &total, &createdLast); err != nil {
			rows.Close()
			return err
		}
		snap.Turns.ByStatus[status] = total
		snap.Turns.Created24h += createdLast
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM turns
		WHERE attempts > 1 AND created_at >= now() - interval '24 hours'`).
		Scan(&snap.Turns.Retried24h); err != nil {
		return err
	}

	return s.collectSkips(ctx, &snap.Turns)
}

// collectSkips группирует turn_skipped по фазе и первой строке причины:
// полная причина содержит сырые ответы модели (21.7) и не годится в метку.
func (s *PgSource) collectSkips(ctx context.Context, turns *TurnsStats) error {
	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(payload->>'phase', ''), coalesce(payload->>'reason', '')
		FROM event_streams
		WHERE type = 'turn_skipped' AND created_at >= now() - interval '24 hours'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct{ phase, reason string }
	counts := make(map[key]int64)
	for rows.Next() {
		var phase, reason string
		if err := rows.Scan(&phase, &reason); err != nil {
			return err
		}
		counts[key{phase: phase, reason: firstLine(reason, 160)}]++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	turns.Skipped24h = make([]SkipCount, 0, len(counts))
	for k, n := range counts {
		turns.Skipped24h = append(turns.Skipped24h, SkipCount{Phase: k.phase, Reason: k.reason, Count: n})
	}
	sort.Slice(turns.Skipped24h, func(i, j int) bool {
		if turns.Skipped24h[i].Count != turns.Skipped24h[j].Count {
			return turns.Skipped24h[i].Count > turns.Skipped24h[j].Count
		}
		if turns.Skipped24h[i].Phase != turns.Skipped24h[j].Phase {
			return turns.Skipped24h[i].Phase < turns.Skipped24h[j].Phase
		}
		return turns.Skipped24h[i].Reason < turns.Skipped24h[j].Reason
	})
	return nil
}

func (s *PgSource) collectModel(ctx context.Context, snap *Snapshot, _ time.Time) error {
	if err := s.pool.QueryRow(ctx,
		`SELECT (now() AT TIME ZONE 'UTC')::date::text`).Scan(&snap.Model.Period); err != nil {
		return err
	}
	limits := map[string]int64{
		runtime.BudgetPoolConversation: s.opts.ConversationCallsLimit,
		runtime.BudgetPoolBackground:   s.opts.BackgroundCallsLimit,
	}
	if s.opts.MaintenanceCallsLimit > 0 {
		limits[runtime.BudgetPoolMaintenance] = s.opts.MaintenanceCallsLimit
	}
	snap.Model.Pools = make(map[string]ModelPool, len(limits))
	for pool, limit := range limits {
		snap.Model.Pools[pool] = ModelPool{Limit: limit}
	}

	rows, err := s.pool.Query(ctx, `
		SELECT pool, reserved_calls, actual_calls, actual_tokens
		FROM budget_counters
		WHERE period = (now() AT TIME ZONE 'UTC')::date`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pool   string
			counts ModelPool
		)
		if err := rows.Scan(&pool, &counts.Reserved, &counts.Calls, &counts.Tokens); err != nil {
			return err
		}
		if known, ok := snap.Model.Pools[pool]; ok {
			known.Reserved = counts.Reserved
			known.Calls = counts.Calls
			known.Tokens = counts.Tokens
			counts = known
		}
		if counts.Limit > 0 {
			used := counts.Reserved
			if counts.Calls > used {
				used = counts.Calls
			}
			counts.Remaining = counts.Limit - used
			if counts.Remaining < 0 {
				counts.Remaining = 0
			}
		}
		snap.Model.Pools[pool] = counts
	}
	return rows.Err()
}

func (s *PgSource) collectMemoryTools(ctx context.Context, snap *Snapshot, _ time.Time) error {
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM event_streams
		WHERE type = 'turn_started'
		  AND payload->>'kind' = 'conversation'
		  AND created_at >= now() - interval '24 hours'`).
		Scan(&snap.MemoryTools.ConversationTurns24h); err != nil {
		return err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT e.stream_id)
		FROM event_streams e
		JOIN event_streams s
		  ON s.stream_id = e.stream_id
		 AND s.type = 'turn_started'
		 AND s.payload->>'kind' = 'conversation'
		WHERE e.type = 'tool_call'
		  AND e.created_at >= now() - interval '24 hours'`).
		Scan(&snap.MemoryTools.TurnsWithTools24h); err != nil {
		return err
	}
	if snap.MemoryTools.ConversationTurns24h > 0 {
		snap.MemoryTools.Share = float64(snap.MemoryTools.TurnsWithTools24h) / float64(snap.MemoryTools.ConversationTurns24h)
	}

	snap.MemoryTools.Reasons = map[string]int64{
		"none":            0,
		"history_trimmed": 0,
		"card_incomplete": 0,
		"rollback":        0,
	}
	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(payload->>'reason', ''), count(*)
		FROM event_streams
		WHERE type = 'turn_plan' AND created_at >= now() - interval '24 hours'
		GROUP BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			reason string
			n      int64
		)
		if err := rows.Scan(&reason, &n); err != nil {
			return err
		}
		if reason == "" {
			reason = "unknown"
		}
		snap.MemoryTools.Reasons[reason] += n
	}
	return rows.Err()
}

func (s *PgSource) collectFirstBubble(ctx context.Context, snap *Snapshot, _ time.Time) error {
	return s.pool.QueryRow(ctx, `
		WITH first_sent AS (
		  SELECT o.turn_id, min(o.updated_at) AS sent_at, t.created_at
		  FROM delivery_outbox o
		  JOIN turns t ON t.id = o.turn_id
		  WHERE o.status = 'sent'
		    AND o.updated_at >= now() - interval '24 hours'
		  GROUP BY o.turn_id, t.created_at
		)
		SELECT count(*),
		       coalesce(percentile_cont(0.5) WITHIN GROUP (
		         ORDER BY extract(epoch FROM (sent_at - created_at)) * 1000), 0),
		       coalesce(percentile_cont(0.95) WITHIN GROUP (
		         ORDER BY extract(epoch FROM (sent_at - created_at)) * 1000), 0)
		FROM first_sent
		WHERE sent_at >= created_at`).
		Scan(&snap.FirstBubble.Count, &snap.FirstBubble.P50Ms, &snap.FirstBubble.P95Ms)
}

func (s *PgSource) collectQueue(ctx context.Context, snap *Snapshot, _ time.Time) error {
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)
		FROM turns WHERE status = 'created'`).
		Scan(&snap.Queue.TurnsCreated, &snap.Queue.OldestTurnAgeSeconds); err != nil {
		return err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM delivery_outbox
		WHERE status IN ('pending', 'sending', 'retry_wait')`).
		Scan(&snap.Queue.OutboxPending); err != nil {
		return err
	}
	return s.pool.QueryRow(ctx, `
		SELECT count(*) FROM intentions
		WHERE status = 'pending'
		  AND not_before <= now()
		  AND (expires_at IS NULL OR expires_at > now())`).
		Scan(&snap.Queue.IntentionsDue)
}

func (s *PgSource) collectDeliveries(ctx context.Context, snap *Snapshot, _ time.Time) error {
	rows, err := s.pool.Query(ctx, `
		SELECT status,
		       count(*),
		       count(*) FILTER (WHERE updated_at >= now() - interval '24 hours')
		FROM delivery_outbox
		GROUP BY status`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status         string
			total, last24h int64
		)
		if err := rows.Scan(&status, &total, &last24h); err != nil {
			return err
		}
		addDelivery(&snap.Deliveries.Total, status, total)
		addDelivery(&snap.Deliveries.Last24h, status, last24h)
	}
	return rows.Err()
}

func addDelivery(counts *DeliveryStatusCounts, status string, n int64) {
	switch status {
	case "sent":
		counts.Sent += n
	case "unknown":
		counts.Unknown += n
	case "failed":
		counts.Failed += n
	case "cancelled":
		counts.Cancelled += n
	case "pending":
		counts.Pending += n
	case "retry_wait":
		counts.RetryWait += n
	case "sending":
		counts.Sending += n
	}
}

func (s *PgSource) collectIntentions(ctx context.Context, snap *Snapshot, _ time.Time) error {
	stats := &snap.Intentions
	stats.ByKind = make(map[string]IntentionKindCounts)
	stats.ProactiveLimitPerChatPerDay = background.MaxProactivePerChatPerDay
	stats.MinGapMinutes = int(background.MinProactiveGap / time.Minute)
	stats.ExplorationsLimit = background.MaxExplorationsPerDay

	rows, err := s.pool.Query(ctx, `
		SELECT kind, status, count(*),
		       count(*) FILTER (WHERE created_at >= now() - interval '24 hours'),
		       count(*) FILTER (WHERE updated_at >= now() - interval '24 hours')
		FROM intentions
		GROUP BY kind, status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			kind, status                    string
			total, createdLast, updatedLast int64
		)
		if err := rows.Scan(&kind, &status, &total, &createdLast, &updatedLast); err != nil {
			rows.Close()
			return err
		}
		counts := stats.ByKind[kind]
		counts.Created24h += createdLast
		switch status {
		case "completed":
			counts.Completed24h += updatedLast
		case "cancelled":
			counts.Cancelled24h += updatedLast
		case "failed":
			counts.Failed24h += updatedLast
		case "pending", "running":
			counts.Pending += total
		}
		stats.ByKind[kind] = counts
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM intentions
		WHERE kind = $1 AND created_at >= date_trunc('day', now())`,
		background.KindExplore).Scan(&stats.ExplorationsToday); err != nil {
		return err
	}

	return s.collectProactiveByChat(ctx, stats)
}

// collectProactiveByChat повторяет политику §14.3: чат израсходовал инициативу,
// если исполненное намерение породило живые доставки (не cancelled/failed).
func (s *PgSource) collectProactiveByChat(ctx context.Context, stats *IntentionsStats) error {
	rows, err := s.pool.Query(ctx, `
		SELECT i.target_chat_id,
		       count(*) FILTER (WHERE i.updated_at >= date_trunc('day', now()))
		FROM intentions i
		WHERE i.kind = ANY($1)
		  AND i.target_chat_id IS NOT NULL
		  AND i.status = 'completed'
		  AND i.related_turn_id IS NOT NULL
		  AND EXISTS (
		    SELECT 1 FROM delivery_outbox d
		    WHERE d.turn_id = i.related_turn_id
		      AND d.status NOT IN ('cancelled', 'failed'))
		GROUP BY i.target_chat_id
		ORDER BY i.target_chat_id`,
		initiativeKinds())
	if err != nil {
		return err
	}
	defer rows.Close()
	stats.ProactiveByChat = make([]ProactiveChat, 0)
	for rows.Next() {
		var chat ProactiveChat
		if err := rows.Scan(&chat.ChatID, &chat.UsedToday); err != nil {
			return err
		}
		chat.Remaining = int64(stats.ProactiveLimitPerChatPerDay) - chat.UsedToday
		if chat.Remaining < 0 {
			chat.Remaining = 0
		}
		stats.ProactiveByChat = append(stats.ProactiveByChat, chat)
	}
	return rows.Err()
}

// initiativeKinds — виды инициатив, способные отправить сообщение вне ответа
// (§14.3). Дублирует список background осознанно: метрика не тянет приватные
// подробности пакета, а список стабилен в спеке.
func initiativeKinds() []string {
	return []string{background.KindReplyLater, background.KindCheckIn, background.KindShare}
}

func (s *PgSource) collectCharacter(ctx context.Context, snap *Snapshot, now time.Time) error {
	baseline := character.Mood{}
	if s.opts.Baseline != nil {
		baseline = s.opts.Baseline(now)
	}

	// Настроение: хранимое значение с затуханием к цели (как в панели), но
	// БЕЗ фиксации затухшего значения — скрейп не пишет в базу.
	mood := baseline
	var (
		valence, arousal, social float64
		updatedAt                time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT valence, arousal, social_energy, updated_at FROM character_state WHERE id = 1`).
		Scan(&valence, &arousal, &social, &updatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Строки ещё нет — базовая линия персоны/расписания.
	case err != nil:
		return err
	default:
		stored := character.Mood{Valence: valence, Arousal: arousal, SocialEnergy: social}
		mood = character.DecayMood(stored, baseline, now.Sub(updatedAt), character.MoodDecayLambdaPerHour)
	}
	snap.Character.Mood = MoodStats{
		Valence:      mood.Valence,
		Arousal:      mood.Arousal,
		SocialEnergy: mood.SocialEnergy,
	}

	// Скука: доросшее значение от последнего события, без CAS-записи (23.1).
	var (
		boredom float64
		from    time.Time
	)
	err = s.pool.QueryRow(ctx,
		`SELECT boredom, boredom_updated_at FROM character_state WHERE id = 1`).
		Scan(&boredom, &from)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		snap.Character.Boredom = 0
	case err != nil:
		return err
	default:
		snap.Character.Boredom = character.GrowBoredom(boredom, from, now, s.opts.Schedule, s.opts.Timezone)
	}
	return nil
}

func (s *PgSource) collectStickers(ctx context.Context, snap *Snapshot, _ time.Time) error {
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE source = 'persona'),
		       count(*) FILTER (WHERE source = 'saved')
		FROM stickers`).
		Scan(&snap.Stickers.Collection, &snap.Stickers.Persona, &snap.Stickers.Saved); err != nil {
		return err
	}
	return s.pool.QueryRow(ctx, `
		SELECT count(*) FROM delivery_outbox
		WHERE payload->>'kind' = 'sticker'
		  AND status = 'sent'
		  AND updated_at >= now() - interval '24 hours'`).
		Scan(&snap.Stickers.Sent24h)
}

func (s *PgSource) collectLifeEvents(ctx context.Context, snap *Snapshot, _ time.Time) error {
	return s.pool.QueryRow(ctx, `
		SELECT count(*) FROM sources
		WHERE kind = 'runtime_action'
		  AND ref LIKE 'life_event:%'
		  AND created_at >= now() - interval '24 hours'`).
		Scan(&snap.LifeEvents.Last24h)
}

func (s *PgSource) collectMemory(ctx context.Context, snap *Snapshot, _ time.Time) error {
	return s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'active'),
		       count(*) FILTER (WHERE status = 'active' AND EXISTS (
		         SELECT 1 FROM memory_embeddings e WHERE e.memory_id = memory_items.id))
		FROM memory_items`).
		Scan(&snap.Memory.Active, &snap.Memory.WithVectors)
}

// firstLine сжимает причину до первой непустой строки и потолка рун: метка
// Prometheus должна оставаться короткой и однострочной.
func firstLine(reason string, limit int) string {
	for _, line := range strings.Split(reason, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > limit {
			return string(runes[:limit-1]) + "…"
		}
		return line
	}
	return "unknown"
}
