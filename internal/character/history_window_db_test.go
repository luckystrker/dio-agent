//go:build db

// БД-допроверка порядка истории 18.12 (приёмка): (1) окно любого LIMIT —
// всегда одни и те же k самых свежих реплик полного детерминированного
// порядка: граница LIMIT не должна «дрожать» между ходами (при равных ts и
// без тай-брейка в окно попадали разные строки); (2) время ранжируется выше
// тай-брейка id: реплика с большим id, но более ранняя по ts не уезжает в
// конец разговора.
package character

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestRecentHistoryEveryWindowIsSuffix — 20 реплик в одну секунду (10 user +
// 10 assistant, id обеих таблиц 1..10): порядок решает только тай-брейк.
// Окно k=1..20 обязано быть суффиксом полного порядка.
func TestRecentHistoryEveryWindowIsSuffix(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	chat := testChatID
	sameTS := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	seedUser := func(updateID int64, text string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, occurred_at)
			VALUES ($1, $2, 'group', 777, 'message', $3, '{}'::jsonb, 'processed', $4)`,
			updateID, chat, text, sameTS); err != nil {
			t.Fatalf("событие %d не вставлено: %v", updateID, err)
		}
	}
	seedAssistant := func(position int, text string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
			VALUES (11, $1, $2, $3::jsonb, 'sent', $4)`,
			position, chat, `{"text":"`+text+`"}`, sameTS); err != nil {
			t.Fatalf("ответ %d не вставлен: %v", position, err)
		}
	}

	for i := 1; i <= 10; i++ {
		seedUser(int64(200+i), fmt.Sprintf("u%d", i))
		seedAssistant(i, fmt.Sprintf("a%d", i))
	}

	// Полный хронологический порядок: при равном ts больший id свежее, при
	// равном id в хронологии сначала user, потом ответ (role ASC в DESC-выборке) —
	// пары идут (u_i, a_i) по росту id.
	wantFull := make([]string, 0, 20)
	for i := 1; i <= 10; i++ {
		wantFull = append(wantFull, fmt.Sprintf("u%d", i), fmt.Sprintf("a%d", i))
	}

	for k := 1; k <= len(wantFull); k++ {
		history, err := RecentHistory(ctx, pool, chat, k, 0)
		if err != nil {
			t.Fatalf("история (окно %d) не читается: %v", k, err)
		}
		want := wantFull[len(wantFull)-k:]
		if len(history) != len(want) {
			t.Fatalf("окно %d: прочитано %d реплик, хочу %d: %+v", k, len(history), len(want), history)
		}
		for i := range want {
			if history[i].Content != want[i] {
				t.Fatalf("окно %d нестабильно: реплика[%d] = %q, хочу %q (весь порядок: %v)",
					k, i, history[i].Content, want[i], contents(history))
			}
		}
	}
}

// TestRecentHistoryTsBeatsId — время решает, id только тай-брейк: ответ
// записан раньше по id (id=1 в outbox), но его время ПОЗЖЕ; реплика человека
// имеет больший id (id=2 в social_events) при более раннем времени.
// Хронологический порядок — сначала ранняя реплика человека, потом поздний
// ответ; порядок «id прежде времени» дал бы наоборот.
func TestRecentHistoryTsBeatsId(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	chat := testChatID

	early := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	late := early.Add(time.Second)

	// Ответ: первая строка outbox — id=1, время позднее.
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
		VALUES (12, 1, $1, $2::jsonb, 'sent', $3)`,
		chat, `{"text":"a-late"}`, late); err != nil {
		t.Fatalf("поздний ответ не вставлен: %v", err)
	}
	// Событие-пустышка без текста занимает id=1 в social_events (в историю не
	// попадает: text IS NOT NULL), чтобы реплика человека получила id=2.
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, occurred_at)
		VALUES (301, $1, 'group', 777, 'message', NULL, '{}'::jsonb, 'processed', $2)`,
		chat, early); err != nil {
		t.Fatalf("пустышка не вставлена: %v", err)
	}
	// Реплика человека: id=2, время раннее.
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, occurred_at)
		VALUES (302, $1, 'group', 777, 'message', $2, '{}'::jsonb, 'processed', $3)`,
		chat, "u-early", early); err != nil {
		t.Fatalf("ранняя реплика не вставлена: %v", err)
	}

	history, err := RecentHistory(ctx, pool, chat, 10, 0)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	want := []string{"u-early", "a-late"}
	if len(history) != len(want) {
		t.Fatalf("прочитано %d реплик (%+v), хочу %d", len(history), history, len(want))
	}
	for i := range want {
		if history[i].Content != want[i] {
			t.Fatalf("порядок «id прежде времени»: реплика[%d] = %q, хочу %q (хронология: время решает, id — только тай-брейк)", i, history[i].Content, want[i])
		}
	}
}

func contents(history []HistoryMessage) []string {
	out := make([]string, 0, len(history))
	for _, m := range history {
		out = append(out, m.Content)
	}
	return out
}
