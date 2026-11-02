// Границы эпизодов (§10.2): «Начальная граница эпизода: 4 часа тишины в DM,
// 1 час в группе или 100 событий. Это граница summary, а не задержка появления
// памяти». Эпизод — отрезок processed-событий чата без внутренних пауз дольше
// границы; он готов к консолидации, когда после него прошла тишина границы
// ИЛИ он набрал 100 событий (разбивка длинных непрерывных серий).
package background

import (
	"context"
	"fmt"
	"time"
)

// Границы эпизода (§10.2, начальные значения спеки).
const (
	// EpisodeSilenceDM — тишина, закрывающая эпизод личного чата.
	EpisodeSilenceDM = 4 * time.Hour
	// EpisodeSilenceGroup — тишина, закрывающая эпизод группы.
	EpisodeSilenceGroup = time.Hour
	// EpisodeMaxEvents — максимум событий одного эпизода (дальше — разбивка).
	EpisodeMaxEvents = 100

	// episodeSelectFactor — потолок выборки событий такта: limit эпизодов
	// умножается на 50 (17.14). Один эпизод — до 100 событий, но выборка идёт
	// по всем чатам вперемешку; запас ×50 покрывает десятки чатов с длинными
	// сериями, не отдавая выборку обратно в полный скан: стоимость такта не
	// должна расти с историей (индекс 000012 держит только необработанный
	// хвост). Хвост сверх потолка подберётся следующими тактами.
	episodeSelectFactor = 50
)

// episodeEvent — событие, участвующее в разметке эпизодов.
type episodeEvent struct {
	ID         int64
	ChatID     int64
	ChatType   string // private | group (по последнему событию чата)
	OccurredAt time.Time
}

// Episode — готовый к консолидации отрезок событий.
type Episode struct {
	ChatID      int64
	ChatType    string
	EventIDs    []int64
	From, To    time.Time
	LastEventID int64 // детерминированная часть id задачи консолидации
}

// episodeBoundary — тишина, закрывающая эпизод чата данного типа.
func episodeBoundary(chatType string) time.Duration {
	if chatType == "private" {
		return EpisodeSilenceDM
	}
	return EpisodeSilenceGroup
}

// readyEpisodes делит события по чатам и паузам и отбирает готовые эпизоды.
// Чистая функция — тестируется без БД.
func readyEpisodes(now time.Time, evs []episodeEvent) []Episode {
	byChat := map[int64][]episodeEvent{}
	var order []int64 // устойчивый порядок чатов
	chatType := map[int64]string{}
	for _, ev := range evs {
		if _, ok := byChat[ev.ChatID]; !ok {
			order = append(order, ev.ChatID)
		}
		byChat[ev.ChatID] = append(byChat[ev.ChatID], ev)
		chatType[ev.ChatID] = ev.ChatType // последнее по порядку обхода
	}

	var out []Episode
	for _, chatID := range order {
		events := byChat[chatID]
		// События приходят из SQL с ORDER BY chat_id, occurred_at, id —
		// здесь только страховка на случай будущих изменений запроса.
		sortEpisodes(events)
		boundary := episodeBoundary(chatType[chatID])

		// Разрез по паузам: тишина между соседними событиями больше границы.
		segments := [][]episodeEvent{}
		cur := []episodeEvent{events[0]}
		for _, ev := range events[1:] {
			if ev.OccurredAt.Sub(cur[len(cur)-1].OccurredAt) > boundary {
				segments = append(segments, cur)
				cur = []episodeEvent{ev}
				continue
			}
			cur = append(cur, ev)
		}
		segments = append(segments, cur)

		for _, seg := range segments {
			// Длинная непрерывная серия режется на куски по 100 событий:
			// «или 100 событий» — граница summary, чат может молчать неделями.
			for len(seg) >= EpisodeMaxEvents {
				out = append(out, newEpisode(chatID, chatType[chatID], seg[:EpisodeMaxEvents]))
				seg = seg[EpisodeMaxEvents:]
			}
			if len(seg) == 0 {
				// Ровно N×100 событий: серия нарезана целиком, хвоста нет.
				continue
			}
			last := seg[len(seg)-1]
			if now.Sub(last.OccurredAt) > boundary {
				out = append(out, newEpisode(chatID, chatType[chatID], seg))
			}
		}
	}
	return out
}

func newEpisode(chatID int64, chatType string, seg []episodeEvent) Episode {
	ids := make([]int64, len(seg))
	for i, ev := range seg {
		ids[i] = ev.ID
	}
	last := seg[len(seg)-1]
	return Episode{
		ChatID:      chatID,
		ChatType:    chatType,
		EventIDs:    ids,
		From:        seg[0].OccurredAt,
		To:          last.OccurredAt,
		LastEventID: last.ID,
	}
}

func sortEpisodes(evts []episodeEvent) {
	for i := 1; i < len(evts); i++ {
		for j := i; j > 0 && evts[j].OccurredAt.Before(evts[j-1].OccurredAt); j-- {
			evts[j], evts[j-1] = evts[j-1], evts[j]
		}
	}
}

// pendingEpisodes читает необработанные processed-события и возвращает готовые
// эпизоды (проверка планировщика — сама модель не вызывает, §14.2). Выборка
// ограничена потолком limit*50 (17.14): лимит эпизодов применялся после
// выборки, и такт сканировал всю историю; теперь хвост сверх потолка
// подбирается следующими тактами.
func pendingEpisodes(ctx context.Context, q queryer, now time.Time, limit int) ([]Episode, error) {
	rows, err := q.Query(ctx, `
		SELECT id, chat_id, chat_type, occurred_at
		FROM social_events
		WHERE status = 'processed' AND summarized_at IS NULL
		ORDER BY chat_id, occurred_at, id
		LIMIT $1`, limit*episodeSelectFactor)
	if err != nil {
		return nil, fmt.Errorf("эпизоды: события не читаются: %w", err)
	}
	defer rows.Close()

	var evs []episodeEvent
	for rows.Next() {
		var ev episodeEvent
		if err := rows.Scan(&ev.ID, &ev.ChatID, &ev.ChatType, &ev.OccurredAt); err != nil {
			return nil, fmt.Errorf("эпизоды: строка не читается: %w", err)
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("эпизоды: выборка не дочитана: %w", err)
	}

	ready := readyEpisodes(now, evs)
	if len(ready) > limit {
		ready = ready[:limit]
	}
	return ready, nil
}
