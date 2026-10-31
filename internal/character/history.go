// Недавний разговор в контексте хода (§11.1 п.5, пункт 8.1): сообщения обеих
// сторон из БД — журнал входящих + доставленные ответы — не только память и не
// пара последних реплик. §13.3/§13.5: в разговор попадают только доставленные
// ответы; pending outbox доступен runtime, но не как состоявшийся опыт.
// Поведение перенесено из TS-справочника src/character/recent-history.ts.
package character

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// HistoryMessage — одна реплика недавнего разговора.
type HistoryMessage struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

// RecentHistory — последние limit сообщений чата (обе стороны) в хронологическом
// порядке. excludeTurnID — события текущего хода: они уже переданы пачкой,
// дублировать их нельзя.
func RecentHistory(ctx context.Context, db DBTX, chatID int64, limit int, excludeTurnID int64) ([]HistoryMessage, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		SELECT role, content FROM (
		  SELECT 'user' AS role, text AS content, occurred_at AS ts
		  FROM social_events
		  WHERE chat_id = $1 AND text IS NOT NULL AND kind IN ('message', 'reply', 'mention', 'sticker', 'photo')
		    AND ($3::bigint = 0 OR turn_id IS NULL OR turn_id <> $3::bigint)
		  UNION ALL
		  SELECT 'assistant' AS role, payload->>'text' AS content, created_at AS ts
		  FROM delivery_outbox
		  WHERE chat_id = $1 AND status = 'sent' AND payload->>'text' IS NOT NULL
		    AND ($3::bigint = 0 OR turn_id <> $3::bigint)
		) timeline
		ORDER BY ts DESC
		LIMIT $2`, chatID, limit, excludeTurnID)
	if err != nil {
		return nil, fmt.Errorf("history: недавний разговор не читается: %w", err)
	}
	defer rows.Close()

	var out []HistoryMessage
	for rows.Next() {
		var m HistoryMessage
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, fmt.Errorf("history: строка не читается: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: недавний разговор не дочитан: %w", err)
	}
	// DESC-выборка разворачивается в хронологический порядок.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// FitHistory подгоняет историю под бюджет объёма: первыми теряются самые
// старые сообщения; самое свежее сохраняется всегда (паритет fitHistory TS).
// Объём считается в символах (рунах), как .length в TS, — не в байтах.
func FitHistory(messages []HistoryMessage, maxChars int) []HistoryMessage {
	var out []HistoryMessage
	total := 0
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		size := utf8.RuneCountInString(message.Content)
		if len(out) > 0 && total+size > maxChars {
			break
		}
		total += size
		out = append([]HistoryMessage{message}, out...)
	}
	return out
}

// historyStream — поток запуска агента с засеянной историей чата: реплики
// прошлых ходов уходят на провод настоящими user/assistant сообщениями ПЕРЕД
// текущей пачкой (BuildMessages ago разворачивает ModelRequest/ModelResponse
// из истории потока). Durable разговор живёт в БД (social_events ∪
// delivery_outbox), стрим одноразовый — на один запуск агента.
func historyStream(hist []HistoryMessage) (*stream.MemoryStream, error) {
	s := stream.NewMemoryStream()
	for _, m := range hist {
		var ev events.Event
		switch m.Role {
		case "user":
			ev = events.ModelRequest{Parts: []events.Input{events.TextInput{Content: m.Content}}}
		case "assistant":
			ev = events.ModelResponse{Message: &events.ModelMessage{Content: m.Content}}
		default:
			continue
		}
		if err := s.Send(ev); err != nil {
			return nil, fmt.Errorf("history: реплика истории не посеяна: %w", err)
		}
	}
	return s, nil
}
