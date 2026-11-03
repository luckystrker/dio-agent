//go:build db

// БД-тест источника метрик (этап 31.2): снимок считается SQL-запросами по
// таблицам-источникам истины (философия №2) — turns/event_streams/
// delivery_outbox/intentions/budget_counters/character_state/stickers/
// memory_items. Каждая цифра проверяется на засеянных строках.
package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

func TestSnapshotFromDatabase(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	seedMetricsFixture(t, ctx, pool)

	src := NewPgSource(pool, SourceOptions{
		Now:                    func() time.Time { return time.Now() },
		Baseline:               func(time.Time) character.Mood { return character.Mood{Valence: 0.2, Arousal: 0.5, SocialEnergy: 0.6} },
		Timezone:               time.UTC,
		ConversationCallsLimit: 100,
		BackgroundCallsLimit:   400,
		MaintenanceCallsLimit:  0,
	})

	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("снимок не собран: %v", err)
	}

	// Решения: answer за час — 1 (фоновые ходы без turn_started в счёт не идут).
	if got := snap.Decisions.Answer.Hour1; got != 1 {
		t.Errorf("answer за час = %d, хочу 1", got)
	}
	if got := snap.Decisions.Ignore.Hour1; got != 0 {
		t.Errorf("ignore за час = %d, хочу 0", got)
	}

	// Ходы и пропуски.
	if got := snap.Turns.ByStatus["applied"]; got != 1 {
		t.Errorf("applied = %d, хочу 1", got)
	}
	if got := snap.Turns.ByStatus["created"]; got != 1 {
		t.Errorf("created = %d, хочу 1", got)
	}
	if got := snap.Turns.Created24h; got != 2 {
		t.Errorf("создано ходов за сутки = %d, хочу 2", got)
	}
	if got := snap.Turns.Retried24h; got != 1 {
		t.Errorf("ретраев за сутки = %d, хочу 1 (attempts=3)", got)
	}
	if len(snap.Turns.Skipped24h) != 1 || snap.Turns.Skipped24h[0].Reason != "модель не настроена" ||
		snap.Turns.Skipped24h[0].Phase != "generate" {
		t.Errorf("пропуски по причинам = %+v", snap.Turns.Skipped24h)
	}

	// Модель: бюджет сегодня по пулам.
	convPool := snap.Model.Pools["conversation"]
	if convPool.Calls != 3 || convPool.Tokens != 1000 || convPool.Reserved != 4 || convPool.Limit != 100 || convPool.Remaining != 96 {
		t.Errorf("conversation пул = %+v", convPool)
	}
	bgPool := snap.Model.Pools["background"]
	if bgPool.Calls != 2 || bgPool.Tokens != 500 || bgPool.Remaining != 398 {
		t.Errorf("background пул = %+v", bgPool)
	}

	// Инструменты памяти: 1 из 2 разговорных ходов, причина из журнала.
	if snap.MemoryTools.ConversationTurns24h != 2 || snap.MemoryTools.TurnsWithTools24h != 1 {
		t.Errorf("инструменты: ходов=%d с инструментами=%d, хочу 2/1",
			snap.MemoryTools.ConversationTurns24h, snap.MemoryTools.TurnsWithTools24h)
	}
	if snap.MemoryTools.Share != 0.5 {
		t.Errorf("доля ходов с инструментами = %v, хочу 0.5", snap.MemoryTools.Share)
	}
	if got := snap.MemoryTools.Reasons["card_incomplete"]; got != 1 {
		t.Errorf("причина card_incomplete = %d, хочу 1", got)
	}

	// Первый пузырь: от создания хода до sent ~2 минуты.
	if snap.FirstBubble.Count != 1 {
		t.Fatalf("замеров первого пузыря = %d, хочу 1", snap.FirstBubble.Count)
	}
	if snap.FirstBubble.P50Ms < 100_000 || snap.FirstBubble.P50Ms > 140_000 {
		t.Errorf("p50 первого пузыря = %v мс, хочу ~120000", snap.FirstBubble.P50Ms)
	}

	// Очередь.
	if snap.Queue.TurnsCreated != 1 || snap.Queue.OutboxPending != 1 || snap.Queue.IntentionsDue != 1 {
		t.Errorf("очередь = %+v, хочу 1/1/1", snap.Queue)
	}
	if snap.Queue.OldestTurnAgeSeconds < 0 || snap.Queue.OldestTurnAgeSeconds > 600 {
		t.Errorf("возраст старейшего хода = %v с", snap.Queue.OldestTurnAgeSeconds)
	}

	// Доставки.
	if snap.Deliveries.Total.Sent != 2 || snap.Deliveries.Total.Pending != 1 {
		t.Errorf("доставки всего = %+v", snap.Deliveries.Total)
	}
	if snap.Deliveries.Last24h.Sent != 2 {
		t.Errorf("доставки за сутки = %+v", snap.Deliveries.Last24h)
	}

	// Инициативы и лимиты §14.3.
	share := snap.Intentions.ByKind["share"]
	if share.Completed24h != 1 || share.Created24h != 1 {
		t.Errorf("share = %+v", share)
	}
	explore := snap.Intentions.ByKind["explore"]
	if explore.Created24h != 1 || explore.Pending != 1 {
		t.Errorf("explore = %+v", explore)
	}
	if len(snap.Intentions.ProactiveByChat) != 1 ||
		snap.Intentions.ProactiveByChat[0].ChatID != 777 ||
		snap.Intentions.ProactiveByChat[0].UsedToday != 1 ||
		snap.Intentions.ProactiveByChat[0].Remaining != 1 {
		t.Errorf("проактивный лимит по чатам = %+v", snap.Intentions.ProactiveByChat)
	}
	if snap.Intentions.ExplorationsToday != 1 || snap.Intentions.ExplorationsLimit != 2 {
		t.Errorf("explore за сегодня/лимит = %d/%d", snap.Intentions.ExplorationsToday, snap.Intentions.ExplorationsLimit)
	}
	if snap.Intentions.ProactiveLimitPerChatPerDay != 2 || snap.Intentions.MinGapMinutes != 240 {
		t.Errorf("политика §14.3 = %d/%d", snap.Intentions.ProactiveLimitPerChatPerDay, snap.Intentions.MinGapMinutes)
	}

	// Скука и настроение.
	if snap.Character.Mood.Valence < 0.19 || snap.Character.Mood.Valence > 0.21 {
		t.Errorf("valence = %v, хочу ~0.2", snap.Character.Mood.Valence)
	}
	if snap.Character.Boredom < 0.39 || snap.Character.Boredom > 0.41 {
		t.Errorf("скука = %v, хочу ~0.4", snap.Character.Boredom)
	}

	// Стикеры, события жизни, память.
	if snap.Stickers.Collection != 2 || snap.Stickers.Persona != 1 || snap.Stickers.Saved != 1 || snap.Stickers.Sent24h != 1 {
		t.Errorf("стикеры = %+v", snap.Stickers)
	}
	if snap.LifeEvents.Last24h != 1 {
		t.Errorf("события жизни за сутки = %d, хочу 1", snap.LifeEvents.Last24h)
	}
	if snap.Memory.Active != 1 || snap.Memory.WithVectors != 1 {
		t.Errorf("память = %+v, хочу 1/1", snap.Memory)
	}
	if snap.Model.Period == "" {
		t.Errorf("период бюджета не заполнен")
	}
}

// seedMetricsFixture засевает по строке на каждую метрику: ходы с событиями
// журнала, доставки, бюджет, намерения, персонажа, стикеры и память.
func seedMetricsFixture(t *testing.T, ctx context.Context, pool *runtime.Pool) {
	t.Helper()
	stmts := []string{
		`INSERT INTO personas (version, name, description, speech_style) VALUES (1, 'Дио', 'описание', 'стиль')`,
		`INSERT INTO character_state (id, persona_version, valence, arousal, social_energy, boredom, boredom_updated_at, updated_at)
		 VALUES (1, 1, 0.2, 0.5, 0.6, 0.4, now(), now())`,
		`INSERT INTO turns (id, chat_id, status, attempts, created_at) VALUES
		   (7001, 777, 'applied', 3, now() - interval '10 minutes'),
		   (7002, 777, 'created', 0, now() - interval '2 minutes')`,
		`INSERT INTO event_streams (stream_id, seq, type, payload) VALUES
		   ('turn:7001', 1, 'turn_started', '{"turn_id":7001,"kind":"conversation"}'),
		   ('turn:7001', 2, 'turn_plan', '{"enabled":true,"reason":"card_incomplete","card_entries":2,"card_fitted":false}'),
		   ('turn:7001', 3, 'tool_call', '{"tool":"memory_search"}'),
		   ('turn:7001', 4, 'model_output', '{"result":{"decision":"answer","messages":["привет"]}}'),
		   ('turn:7002', 1, 'turn_started', '{"turn_id":7002,"kind":"conversation"}'),
		   ('turn:7002', 2, 'turn_skipped', '{"turn_id":7002,"phase":"generate","reason":"модель не настроена"}')`,
		`INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at, updated_at) VALUES
		   (7001, 0, 777, '{"kind":"message","text":"привет"}', 'sent', now() - interval '10 minutes', now() - interval '8 minutes'),
		   (7001, 1, 777, '{"kind":"sticker","file_id":"f1"}', 'sent', now() - interval '10 minutes', now() - interval '8 minutes'),
		   (7002, 0, 777, '{"kind":"message","text":"ещё"}', 'pending', now() - interval '2 minutes', now() - interval '2 minutes')`,
		`INSERT INTO budget_counters (pool, period, reserved_calls, actual_calls, actual_tokens) VALUES
		   ('conversation', (now() AT TIME ZONE 'UTC')::date, 4, 3, 1000),
		   ('background', (now() AT TIME ZONE 'UTC')::date, 2, 2, 500)`,
		`INSERT INTO provenance (kind, ref, scopes) VALUES ('runtime_action', 'metrics-test', ARRAY['internal'])`,
		`INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, status, not_before, dedupe_key, related_turn_id, created_at, updated_at)
		 SELECT 'share', 777, 'пластинки', 'тест', id, 'completed', now(), 'share:metrics-test', 7001, now() - interval '1 hour', now() - interval '30 minutes'
		 FROM provenance WHERE ref = 'metrics-test'`,
		`INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, status, not_before, dedupe_key, related_turn_id, created_at, updated_at)
		 SELECT 'explore', NULL, 'любопытство', 'тест', id, 'pending', now(), 'explore:metrics-test', NULL, now(), now()
		 FROM provenance WHERE ref = 'metrics-test'`,
		`INSERT INTO stickers (file_id, file_unique_id, emoji, source) VALUES
		   ('f1', 'u1', '😀', 'persona'), ('f2', 'u2', '😎', 'saved')`,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, status)
		 SELECT 'experience', 'приготовил чай', 'public', id, 'active' FROM provenance WHERE ref = 'metrics-test'`,
		`INSERT INTO memory_embeddings (memory_id, model, embedding)
		 SELECT id, 'bge-m3', '[0.1,0.2,0.3]' FROM memory_items WHERE text = 'приготовил чай'`,
		`INSERT INTO sources (kind, ref, created_at) VALUES ('runtime_action', 'life_event:1', now() - interval '1 hour')`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("фикстура не засеяна (%s): %v", stmt, err)
		}
	}
}
