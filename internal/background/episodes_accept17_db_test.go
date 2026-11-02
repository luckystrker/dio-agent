//go:build db

// Приёмка 17.14: потолок выборки поведенчески. Чат с меньшим id держит
// ровно потолок (limit×50) необработанных событий — такт видит только его,
// события следующего чата в выборку не попадают; после консолидации
// (summarized_at проставлен) следующий такт подбирает хвост. До фикса
// лимит применялся после выборки, и стоимость такта росла с историей.
package background

import (
	"testing"
	"time"
)

// TestAcceptPendingEpisodesCeilingAndTail — потолок выборки ограничивает
// чтение; хвост подбирается следующим тактом после консолидации.
func TestAcceptPendingEpisodesCeilingAndTail(t *testing.T) {
	f, ctx := newFixture(t)

	// Чат 1 (меньший id): 500 событий без тишины (интервал 1 мин) — ровно
	// потолок limit×50 при limit=10; нарезка — 5 эпизодов по 100 событий.
	const chat1, chat2 = int64(9101), int64(9102)
	for i := 0; i < 500; i++ {
		f.seedEvent(t, ctx, chat1, "private", "серия чата 1",
			f.now.Add(-time.Duration(501-i)*time.Minute))
	}
	// Чат 2: 100 событий без тишины, последнее давно — эпизод готов, но
	// ORDER BY chat_id при заполненном потолке до него не доходит.
	for i := 0; i < 100; i++ {
		f.seedEvent(t, ctx, chat2, "private", "серия чата 2",
			f.now.Add(-time.Duration(3000-i)*time.Minute))
	}

	ready, err := pendingEpisodes(ctx, f.pool, f.now, 10)
	if err != nil {
		t.Fatalf("pendingEpisodes: %v", err)
	}
	if len(ready) == 0 {
		t.Fatal("эпизоды чата 1 не найдены")
	}
	for _, ep := range ready {
		if ep.ChatID == chat2 {
			t.Fatalf("потолок выборки не работает: эпизод чата %d прочитан поверх %d событий чата %d",
				chat2, 500, chat1)
		}
	}
	if len(ready) > 5 {
		t.Fatalf("эпизодов %d — больше, чем даёт потолок в 500 событий (5 по 100)", len(ready))
	}

	// Консолидация закрыла события чата 1 — следующий такт подбирает хвост.
	if _, err := f.pool.Exec(ctx,
		`UPDATE social_events SET summarized_at = now() WHERE chat_id = $1`, chat1); err != nil {
		t.Fatalf("summarized_at не проставлен: %v", err)
	}
	ready2, err := pendingEpisodes(ctx, f.pool, f.now, 10)
	if err != nil {
		t.Fatalf("pendingEpisodes (второй такт): %v", err)
	}
	if len(ready2) != 1 || ready2[0].ChatID != chat2 {
		t.Fatalf("хвост не подобрался следующим тактом: %+v", ready2)
	}
}
