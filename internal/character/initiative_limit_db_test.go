//go:build db

// БД-тесты этапа 33.2: когда дневной лимит инициатив личного чата исчерпан
// (§14.3), панель говорит модели «больше нельзя», а применение хода не
// создаёт новые check_in/share. reply_later (defer) дневным лимитом не
// ограничивается — см. stage41_db_test.go (41.2 M6).
package character

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// seedDeliveredInitiative — исполненная инициатива чата с живой доставкой:
// ровно так ProactiveUsage считает израсходованный дневной лимит.
func seedDeliveredInitiative(t *testing.T, ctx context.Context, f *fixture, id, turnID int64, kind string) {
	t.Helper()
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, f.pool,
		memory.SourceJournalEvent, fmt.Sprintf("seed:limit:%d", id), []string{testScopeID})
	if err != nil {
		t.Fatalf("provenance инициативы не создан: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO intentions (id, kind, target_chat_id, topic, reason, provenance_id,
		                        status, not_before, expires_at, dedupe_key, related_turn_id, updated_at)
		VALUES ($1, $2, $3, $4, 'тестовая инициатива', $5, 'completed', now(),
		        now() + interval '1 day', $6, $7, now())`,
		id, kind, testChatID, fmt.Sprintf("тема %d", id), provenanceID,
		fmt.Sprintf("seed-limit-%d", id), turnID); err != nil {
		t.Fatalf("исполненная инициатива не вставлена: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status)
		VALUES ($1, 0, $2, '{"kind":"message","text":"инициатива"}'::jsonb, 'sent')`,
		turnID, testChatID); err != nil {
		t.Fatalf("доставка инициативы не вставлена: %v", err)
	}
}

// seedExhaustedInitiativeLimit — два исполненных инициативных хода в чате.
func seedExhaustedInitiativeLimit(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	seedDeliveredInitiative(t, ctx, f, 7001, 70001, "check_in")
	seedDeliveredInitiative(t, ctx, f, 7002, 70002, "share")
}

func countIntentions(t *testing.T, ctx context.Context, f *fixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE target_chat_id = $1`, testChatID).Scan(&n); err != nil {
		t.Fatalf("намерения не посчитаны: %v", err)
	}
	return n
}

func TestApplyKeepsOneInitiativeAtDailyLimit(t *testing.T) {
	f, ctx := newFixture(t)
	seedExhaustedInitiativeLimit(t, ctx, f)
	f.seedTurn(t, ctx, 801, "привет")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer",` +
		`"messages":["ок"],"intentions":[` +
		`{"kind":"check_in","topic":"проверить как дела","reason":"давно не писали","delay_hours":2},` +
		`{"kind":"share","topic":"нашёл трек","reason":"понравилось","delay_hours":1}]}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 801, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}
	if len(out.Apply.IntentionIDs) != 1 {
		t.Fatalf("при исчерпанном лимите должно остаться одно намерение, получено: %v", out.Apply.IntentionIDs)
	}
	if len(out.Apply.LimitedIntentions) != 1 {
		t.Fatalf("ограниченных намерений %d, хочу 1: %+v", len(out.Apply.LimitedIntentions), out.Apply.LimitedIntentions)
	}
	if n := countIntentions(t, ctx, f); n != 3 {
		t.Fatalf("намерений в чате %d, хочу 2 исполненных + 1 ожидающее: %v", n, n)
	}
}

func TestPanelShowsInitiativeLimitReached(t *testing.T) {
	f, ctx := newFixture(t)
	// Тип чата панель читает из событий: живой чат всегда их имеет.
	f.seedHistoryEvent(t, ctx, 9401, 401, "message", "привет", time.Now().Add(-time.Minute))
	batch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}

	before, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	const line = "на сегодня инициатив больше нельзя — не планируй check_in/share"
	if strings.Contains(before.StatePanel, line) {
		t.Fatalf("панель говорит о лимите, когда он не исчерпан:\n%s", before.StatePanel)
	}

	seedExhaustedInitiativeLimit(t, ctx, f)
	after, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст после лимита не собран: %v", err)
	}
	if !strings.Contains(after.StatePanel, line) {
		t.Fatalf("в панели нет строки о лимите инициатив:\n%s", after.StatePanel)
	}
}
