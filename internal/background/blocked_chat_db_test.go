//go:build db

// БД-тесты пометки «чат заблокировал бота» в фоне (этап 38.7): инициативы в
// такой чат не исполняются, а поиск интереса от скуки не выбирает его как
// аудиторию находки.
package background

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// insertRawIntention — намерение в обход гейта применения (пометка могла
// появиться позже создания): pending, срок — сейчас.
func insertRawIntention(t *testing.T, ctx context.Context, f *fixture, kind, topic string, chatID int64) int64 {
	t.Helper()
	var provenanceID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO provenance (kind, ref, scopes) VALUES ('runtime_action', $1, ARRAY[$2]) RETURNING id`,
		"test:blocked:"+topic, "chat:"+strconv.FormatInt(chatID, 10)).Scan(&provenanceID); err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	var id int64
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, not_before, expires_at, dedupe_key)
		VALUES ($1, $2, $3, 'тест 38.7', $4, now() - interval '1 minute', now() + interval '1 hour', $5)
		RETURNING id`, kind, chatID, topic, provenanceID, "test-blocked-"+topic).Scan(&id); err != nil {
		t.Fatalf("намерение не создано: %v", err)
	}
	return id
}

func TestBlockedChatInitiativeIsSkipped(t *testing.T) {
	f, ctx := newFixture(t)
	script := f.withModel(t) // ни одного модельного вызова быть не должно

	// Пометка ставится первой, намерение — в обход гейта создания (гонка:
	// строка уже была в очереди/claim'е, когда пришёл 403).
	if _, err := telegram.MarkChatBlocked(ctx, f.pool, bgChatID, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("чат не помечен: %v", err)
	}
	id := insertRawIntention(t, ctx, f, KindCheckIn, "как сам", bgChatID)

	claimed, err := f.intentions.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("claim не удался: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("claim взял не то намерение: %+v", claimed)
	}
	skip, reason, err := f.sched.runGates(ctx, claimed[0])
	if err != nil {
		t.Fatalf("гейты упали: %v", err)
	}
	if !skip {
		t.Fatal("инициатива в заблокированный чат обязана пропускаться")
	}
	if !strings.Contains(reason, "заблокировал") {
		t.Fatalf("причина пропуска не про блокировку: %q", reason)
	}
	if got := len(script.Calls()); got != 0 {
		t.Fatalf("модельных вызовов %d, хочу 0 — в закрытую дверь не стучим", got)
	}
}

func TestBoredomIgnoresBlockedChat(t *testing.T) {
	f, ctx := newFixture(t)
	oldChat := int64(-200500)
	f.seedEventStatus(t, ctx, oldChat, "private", "старый живой чат", f.now.Add(-3*time.Hour), "journal_only")
	// Последний личный чат — заблокировавший бота.
	f.seedEventStatus(t, ctx, bgChatID, "private", "закрытый чат", f.now.Add(-time.Hour), "journal_only")
	if _, err := telegram.MarkChatBlocked(ctx, f.pool, bgChatID, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("чат не помечен: %v", err)
	}

	chatID, err := f.sched.latestPrivateChat(ctx)
	if err != nil {
		t.Fatalf("личный чат не выбран: %v", err)
	}
	if chatID == bgChatID {
		t.Fatal("скука выбрала чат, заблокировавший бота, — share туда не дойдёт")
	}
	if chatID != oldChat {
		t.Fatalf("выбран чат %d, хочу живой %d", chatID, oldChat)
	}
}
