// Недавний разговор в контексте хода (§11.1 п.5, пункт 8.1): сообщения обеих
// сторон из БД — журнал входящих + доставленные ответы — не только память и не
// пара последних реплик. §13.3/§13.5: в разговор попадают только доставленные
// ответы; pending outbox доступен runtime, но не как состоявшийся опыт.
// pending-входящие (событие ещё в буфере, 18.2) тоже не «состоявшийся опыт»:
// они придут пачкой своего хода и не должны дублироваться историей.
// Поведение перенесено из TS-справочника src/character/recent-history.ts.
package character

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// HistoryMessage — одна реплика недавнего разговора.
type HistoryMessage struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
	// At — абсолютное время реплики (occurred_at входящего / created_at
	// доставленного ответа). Метки истории абсолютные: относительные формулы
	// («2 часа назад») сделали бы историю разной между ходами и сломали кэш.
	At time.Time `json:"at,omitempty"`
	// Author — имя автора user-реплики (actor_name; нет — user:<id>).
	// У assistant-реплик пусто: метка времени без автора не провоцирует
	// модель писать «Дио:» в ответе.
	Author string `json:"author,omitempty"`
}

// RecentHistory — последние limit сообщений чата (обе стороны) в хронологическом
// порядке. excludeTurnID — события текущего хода: они уже переданы пачкой,
// дублировать их нельзя. Порядок при равном ts задаётся полностью: больший id
// считается свежее, роль — финальный тай-брейк (id двух таблиц — независимые
// последовательности и могут совпасть). role ASC в DESC-выборке ставит user
// первым в хронологии после разворота: при совпавших ts и id реплика человека
// идёт перед ответом, а не наоборот; без тай-брейка соседние реплики равного
// времени перетасовывались между ходами и ломали кэш префикса провайдера. Обрезано ли окно лимитом сообщений — не сообщается
// (RecentHistoryWindow для сборки контекста).
func RecentHistory(ctx context.Context, db DBTX, chatID int64, limit int, excludeTurnID int64) ([]HistoryMessage, error) {
	out, _, err := RecentHistoryWindow(ctx, db, chatID, limit, excludeTurnID)
	return out, err
}

// RecentHistoryWindow — то же, что RecentHistory, но сообщает, урезала ли
// выборку граница окна: сборке контекста нужно знать, видит ли модель разговор
// целиком (решение 2026-11-02: целая история + карточка памяти = ход без
// инструментов памяти). Запрашивается limit+1 строка — лишняя отбрасывается.
func RecentHistoryWindow(ctx context.Context, db DBTX, chatID int64, limit int, excludeTurnID int64) ([]HistoryMessage, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	rows, err := db.Query(ctx, `
		SELECT role, content, ts, author FROM (
		  SELECT 'user' AS role,
		         CASE WHEN kind = 'photo'
		              THEN COALESCE(NULLIF(btrim(text), ''), '[фото]')
		              ELSE text END AS content,
		         occurred_at AS ts,
		         id,
		         COALESCE(NULLIF(btrim(actor_name), ''), 'user:' || COALESCE(actor_id::text, '0')) AS author
		  FROM social_events
		  WHERE chat_id = $1 AND (text IS NOT NULL OR kind = 'photo')
		    AND kind IN ('message', 'reply', 'mention', 'sticker', 'photo', 'reaction')
		    AND status <> 'pending'
		    AND ($3::bigint = 0 OR turn_id IS NULL OR turn_id <> $3::bigint)
		  UNION ALL
		  SELECT 'assistant' AS role, payload->>'text' AS content, created_at AS ts, id, '' AS author
		  FROM delivery_outbox
		  WHERE chat_id = $1 AND status = 'sent' AND payload->>'text' IS NOT NULL
		    AND ($3::bigint = 0 OR turn_id <> $3::bigint)
		) timeline
		ORDER BY ts DESC, id DESC, role ASC
		LIMIT $2`, chatID, limit+1, excludeTurnID)
	if err != nil {
		return nil, false, fmt.Errorf("history: недавний разговор не читается: %w", err)
	}
	defer rows.Close()

	out := make([]HistoryMessage, 0, limit)
	for rows.Next() {
		var m HistoryMessage
		if err := rows.Scan(&m.Role, &m.Content, &m.At, &m.Author); err != nil {
			return nil, false, fmt.Errorf("history: строка не читается: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("history: недавний разговор не дочитан: %w", err)
	}
	// DESC-выборка разворачивается в хронологический порядок.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	// limit+1-я строка — доказательство, что окно урезало разговор.
	trimmedByLimit := false
	if len(out) > limit {
		out = out[len(out)-limit:]
		trimmedByLimit = true
	}
	return out, trimmedByLimit, nil
}

// FitHistory подгоняет историю под бюджет объёма: первыми теряются самые
// старые сообщения; самое свежее сохраняется всегда (паритет fitHistory TS).
// Объём считается в символах (рунах), как .length в TS, — не в байтах.
// С 2026-11-02 в объём входит и рендер реплики на проводе: метка времени
// (17 рун) и автор user-реплики (имя + «: ») — иначе фактический запрос
// превышал MODEL_HISTORY_MAX_CHARS. Результат — суффикс входа, поэтому окно
// считается одним проходом с конца и без копирования (вставка в начало среза
// копировала буфер на каждую реплику, O(n²)).
func FitHistory(messages []HistoryMessage, maxChars int) []HistoryMessage {
	last := len(messages) - 1
	total := 0
	for i := last; i >= 0; i-- {
		size := historyMessageCost(messages[i])
		// Самое свежее (первая итерация) живёт всегда — бюджет не спрашивают.
		if i < last && total+size > maxChars {
			return messages[i+1:]
		}
		total += size
	}
	return messages
}

// historyMessageCost — объём реплики на проводе: текст + абсолютная метка
// «[пн 02.11 17:02] » (16 рун со скобками и пробелом) + автор «Имя: » у
// user-реплик. Без времени (нулевое At) — только текст (ручные вызовы/тесты).
func historyMessageCost(m HistoryMessage) int {
	size := utf8.RuneCountInString(m.Content)
	if m.At.IsZero() {
		return size
	}
	size += utf8.RuneCountInString(formatHistoryTime(m.At, nil)) + 3 // «[» + метка + «] »
	if m.Role == "user" && m.Author != "" {
		size += utf8.RuneCountInString(sanitizePanelValue(m.Author, 80)) + len(": ")
	}
	return size
}

// historyStream — поток запуска агента с засеянной историей чата: реплики
// прошлых ходов уходят на провод настоящими user/assistant сообщениями ПЕРЕД
// текущей пачкой (BuildMessages ago разворачивает ModelRequest/ModelResponse
// из истории потока). Durable разговор живёт в БД (social_events ∪
// delivery_outbox), стрим одноразовый — на один запуск агента.
//
// Каждая реплика несёт абсолютную метку времени в поясе персонажа; у
// user-реплик ещё и автор: `[пн 02.11 17:02] Данил: текст`. У assistant —
// только метка: `[пн 02.11 17:03] текст` (метка без автора не провоцирует
// модель дописывать «Дио:»). Формат детерминирован по данным БД → история
// байт-стабильна между ходами.
func historyStream(hist []HistoryMessage, loc *time.Location) (*stream.MemoryStream, error) {
	s := stream.NewMemoryStream()
	for _, m := range hist {
		content := renderHistoryLine(m, loc)
		var ev events.Event
		switch m.Role {
		case "user":
			ev = events.ModelRequest{Parts: []events.Input{events.TextInput{Content: content}}}
		case "assistant":
			ev = events.ModelResponse{Message: &events.ModelMessage{Content: content}}
		default:
			continue
		}
		if err := s.Send(ev); err != nil {
			return nil, fmt.Errorf("history: реплика истории не посеяна: %w", err)
		}
	}
	return s, nil
}

// renderHistoryLine добавляет к реплике абсолютную метку и автора. Нулевое
// время (старые фикстуры/ручные вызовы) — метки нет, текст без изменений.
func renderHistoryLine(m HistoryMessage, loc *time.Location) string {
	content := m.Content
	if m.At.IsZero() {
		return content
	}
	stamp := "[" + formatHistoryTime(m.At, loc) + "] "
	if author := sanitizePanelValue(m.Author, 80); m.Role == "user" && author != "" {
		return stamp + author + ": " + content
	}
	return stamp + content
}
