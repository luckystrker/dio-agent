//go:build db

// Пункт 16.1 на реальном стыке: SQL pendingEpisodes (ORDER BY chat_id,
// occurred_at, id) → readyEpisodes. До фикса ровно 100 processed-событий
// одного чата паниковали index out of range прямо в такте планировщика
// (горутина без recover — падал весь процесс). Запуск против Postgres из
// docker-compose: TEST_DATABASE_URL=... go test -tags db ./internal/background/
package background

import (
	"testing"
	"time"
)

// TestPendingEpisodesExactHundredDB — ровно 100 свежих processed-событий
// одного чата (тишины нет): «или 100 событий» §10.2 делает эпизод готовым
// без паузы, нарезка не паникует, эпизод один и из тех самых событий.
func TestPendingEpisodesExactHundredDB(t *testing.T) {
	f, ctx := newFixture(t)

	// Серия без тишины: интервал 1 мин, последнее событие минуту назад.
	var oldest, newest int64
	for i := 0; i < 100; i++ {
		at := f.now.Add(-time.Duration(100-i) * time.Minute)
		id := f.seedEvent(t, ctx, bgChatID, "private", "сообщение серии без тишины", at)
		if i == 0 {
			oldest = id
		}
		newest = id
	}

	ready, err := pendingEpisodes(ctx, f.pool, f.now, 10)
	if err != nil {
		t.Fatalf("pendingEpisodes не должен падать на ровно 100 событиях: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("хочу 1 готовый эпизод, получено %d: %+v", len(ready), ready)
	}
	ep := ready[0]
	if n := len(ep.EventIDs); n != EpisodeMaxEvents {
		t.Fatalf("эпизод не из %d событий: %d", EpisodeMaxEvents, n)
	}
	if ep.ChatID != bgChatID {
		t.Fatalf("эпизод чужого чата: %d", ep.ChatID)
	}
	if ep.LastEventID != newest {
		t.Fatalf("LastEventID=%d, хочу самое свежее событие %d", ep.LastEventID, newest)
	}
	if ep.EventIDs[0] != oldest || ep.EventIDs[len(ep.EventIDs)-1] != newest {
		t.Fatalf("эпизод не охватывает серию целиком: первый %d, последний %d (сеял %d..%d)",
			ep.EventIDs[0], ep.EventIDs[len(ep.EventIDs)-1], oldest, newest)
	}
}
