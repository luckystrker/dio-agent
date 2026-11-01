// Приёмочные проверки пункта 16.1 со свежего контекста: паника нарезки на
// ровно N×100 событий. Сценарии, не покрытые TestReadyEpisodesExactChunks
// (группа, совпадение двух границ, несколько чатов с ровно N×100 в одном
// прогоне). Чистая функция — без БД.
package background

import (
	"testing"
	"time"
)

// series — непрерывная серия из n событий чата: интервал gap « границы,
// последнее событие в lastAt (тишину задаёт вызывающий).
func series(chatID int64, chatType string, n int, gap time.Duration, lastAt time.Time, idBase int64) []episodeEvent {
	evs := make([]episodeEvent, 0, n)
	for i := 0; i < n; i++ {
		evs = append(evs, episodeEvent{
			ID:         idBase + int64(i),
			ChatID:     chatID,
			ChatType:   chatType,
			OccurredAt: lastAt.Add(-time.Duration(n-1-i) * gap),
		})
	}
	return evs
}

// TestReadyEpisodesExactHundredVariants — «или 100 событий» §10.2 работает
// и в группе (граница тишины 1 ч, а не 4 ч), и когда обе границы совпадают
// (100 событий И тишина уже прошла — эпизод ровно один, не два), и не
// роняет соседние чаты того же прогона (до фикса паника одного чата
// убивала весь такт планировщика — горутина без recover, процесс целиком).
func TestReadyEpisodesExactHundredVariants(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	t.Run("группа: ровно 100 — один готовый кусок, без паники", func(t *testing.T) {
		evs := series(300, "group", 100, time.Minute, now.Add(-time.Minute), 1)
		got := readyEpisodes(now, evs)
		if len(got) != 1 || len(got[0].EventIDs) != EpisodeMaxEvents {
			t.Fatalf("хочу 1 кусок из %d событий, получено %d кусков: %+v",
				EpisodeMaxEvents, len(got), got)
		}
		if got[0].ChatType != "group" {
			t.Fatalf("тип чата потерян: %q", got[0].ChatType)
		}
	})

	t.Run("ровно 100 и тишина уже прошла — эпизод один, не два", func(t *testing.T) {
		// Обе границы §10.2 сработали разом: 100 событий и 5 часов тишины
		// после последнего. Нарезка уже отдала эпизод — пустой хвост не
		// должен ни паниковать, ни додать второй (пустой) эпизод.
		evs := series(400, "private", 100, time.Minute, now.Add(-5*time.Hour), 1)
		got := readyEpisodes(now, evs)
		if len(got) != 1 {
			t.Fatalf("хочу ровно 1 эпизод, получено %d: %+v", len(got), got)
		}
		if n := len(got[0].EventIDs); n != EpisodeMaxEvents {
			t.Fatalf("эпизод не из %d событий: %d", EpisodeMaxEvents, n)
		}
	})

	t.Run("два чата с ровно N×100 в одном прогоне — оба нарезаны", func(t *testing.T) {
		// Личка с ровно 100 и группа с ровно 200 вперемешку: до фикса паника
		// первого же чата роняла готовность остальных. Порядок чатов —
		// по первому событию в выборке.
		a := series(500, "private", 100, time.Minute, now.Add(-time.Minute), 1)
		b := series(600, "group", 200, time.Minute, now.Add(-time.Minute), 1000)
		var evs []episodeEvent
		for i := 0; i < 200; i++ {
			if i < len(a) {
				evs = append(evs, a[i])
			}
			evs = append(evs, b[i])
		}
		got := readyEpisodes(now, evs)
		if len(got) != 3 {
			t.Fatalf("хочу 3 готовых куска (1 личка + 2 группа), получено %d: %+v", len(got), got)
		}
		if got[0].ChatID != 500 || got[1].ChatID != 600 || got[2].ChatID != 600 {
			t.Fatalf("порядок чатов сломан: %+v", []int64{got[0].ChatID, got[1].ChatID, got[2].ChatID})
		}
		for i, ep := range got {
			if n := len(ep.EventIDs); n != EpisodeMaxEvents {
				t.Fatalf("кусок %d не из %d событий: %d", i, EpisodeMaxEvents, n)
			}
		}
		// Куски группы подряд: [1000..1099] [1100..1199].
		if got[1].EventIDs[0] != 1000 || got[1].LastEventID != 1099 ||
			got[2].EventIDs[0] != 1100 || got[2].LastEventID != 1199 {
			t.Fatalf("куски группы не подряд: %+v", got[1:])
		}
	})
}
