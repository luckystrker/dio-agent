//go:build db

// БД-тест порядка cooldown-фильтра и лимита консолидации (§10.2/§14.2, блок
// 17.4): лимит такта применялся ДО проверки cooldown — два остывающих эпизода
// с наименьшими chat_id занимали оба слота, остальные чаты не консолидировались
// сутки. Фильтр обязан идти до обрезки.
package background

import (
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// TestConsolidationCooldownDoesNotStarveOtherChats — два остывающих эпизода
// (cooldown, наименьшие chat_id) + готовый эпизод чужого чата: готовый
// консолидируется в тот же такт, слоты не сгорают на остывающих.
func TestConsolidationCooldownDoesNotStarveOtherChats(t *testing.T) {
	f, ctx := newFixture(t)
	script := f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Эпизод чужого чата сведён, пока соседи остывают.", "Чат жив, тема ясна")}},
	)

	// Остывающие — младшие chat_id (при лимите 2 занимали бы оба слота).
	coolA := int64(-301000)
	coolB := int64(-300999)
	// Готовый — старший chat_id: без фикса отрезался обрезкой лимита.
	ready := int64(-300500)

	idsA, _ := f.seedEpisode(t, ctx, coolA, "private", 3, f.now.Add(-5*time.Hour))
	idsB, _ := f.seedEpisode(t, ctx, coolB, "private", 3, f.now.Add(-5*time.Hour))
	idsR, _ := f.seedEpisode(t, ctx, ready, "private", 3, f.now.Add(-5*time.Hour))

	epA := Episode{ChatID: coolA, ChatType: "private", EventIDs: idsA, LastEventID: idsA[len(idsA)-1]}
	epB := Episode{ChatID: coolB, ChatType: "private", EventIDs: idsB, LastEventID: idsB[len(idsB)-1]}
	if err := f.sched.setConsolidationCooldown(ctx, epA); err != nil {
		t.Fatalf("cooldown A не поставлен: %v", err)
	}
	if err := f.sched.setConsolidationCooldown(ctx, epB); err != nil {
		t.Fatalf("cooldown B не поставлен: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if got := len(script.Calls()); got != 1 {
		t.Fatalf("готовый эпизод должен консолидироваться в тот же такт (лимит не занят остывающими), вызовов модели %d", got)
	}

	unsummarized := func(ids []int64) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).Scan(&n); err != nil {
			t.Fatalf("события не читаются: %v", err)
		}
		return n
	}
	if n := unsummarized(idsR); n != 0 {
		t.Fatalf("готовый эпизод не сведён: не помечено %d событий", n)
	}
	if n := unsummarized(idsA); n != len(idsA) {
		t.Fatalf("остывающий эпизод A тронут консолидацией: не помечено %d, хочу все %d", n, len(idsA))
	}
	if n := unsummarized(idsB); n != len(idsB) {
		t.Fatalf("остывающий эпизод B тронут консолидацией: не помечено %d, хочу все %d", n, len(idsB))
	}
}

// TestConsolidationCooldownThreeCoolingStillFillsBothSlots — остывающих больше
// лимита (3 при лимите 2, младшие chat_id): оба слота такта всё равно уходят
// готовым эпизодам чужих чатов — cooldown-фильтр не даёт остывающим занимать
// слоты до проверки (17.4).
func TestConsolidationCooldownThreeCoolingStillFillsBothSlots(t *testing.T) {
	f, ctx := newFixture(t)
	script := f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Первый готовый эпизод сведён, соседи остывают.", "Чат жив, тема ясна")}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Второй готовый эпизод сведён, соседи остывают.", "Чат жив, тема ясна")}},
	)

	// Остывающие — три младших chat_id (до фикса занимали оба слота лимита 2).
	coolChats := []int64{-303000, -302999, -302998}
	coolIDs := make([][]int64, 0, len(coolChats))
	for _, chat := range coolChats {
		ids, _ := f.seedEpisode(t, ctx, chat, "private", 3, f.now.Add(-5*time.Hour))
		coolIDs = append(coolIDs, ids)
		if err := f.sched.setConsolidationCooldown(ctx, Episode{
			ChatID: chat, ChatType: "private", EventIDs: ids, LastEventID: ids[len(ids)-1],
		}); err != nil {
			t.Fatalf("cooldown чата %d не поставлен: %v", chat, err)
		}
	}
	// Готовые — старшие chat_id: без фикса отрезались обрезкой лимита.
	readyA, _ := f.seedEpisode(t, ctx, -302000, "private", 3, f.now.Add(-5*time.Hour))
	readyB, _ := f.seedEpisode(t, ctx, -301999, "private", 3, f.now.Add(-5*time.Hour))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if got := len(script.Calls()); got != 2 {
		t.Fatalf("оба слота такта должны уходить готовым эпизодам (остывающих больше лимита), вызовов модели %d", got)
	}

	unsummarized := func(ids []int64) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).Scan(&n); err != nil {
			t.Fatalf("события не читаются: %v", err)
		}
		return n
	}
	if n := unsummarized(readyA); n != 0 {
		t.Fatalf("готовый эпизод A не сведён: не помечено %d событий", n)
	}
	if n := unsummarized(readyB); n != 0 {
		t.Fatalf("готовый эпизод B не сведён: не помечено %d событий", n)
	}
	for i, ids := range coolIDs {
		if n := unsummarized(ids); n != len(ids) {
			t.Fatalf("остывающий эпизод %d тронут консолидацией: не помечено %d, хочу все %d", i, n, len(ids))
		}
	}
}
