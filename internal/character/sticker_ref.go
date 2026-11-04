// Разбор ссылки на стикер из ответа модели (этап 33.1): модель просит стикер
// номером из коллекции (#N) или эмодзи из неё. Живые логи 2026-11-03/04
// показали, что модель почти в каждом ходе заполняет поле и пишет туда либо
// эмодзи, либо служебное «null»/«none»/«не используется…». Служебное — не
// запрос стикера (тихо), эмодзи — поиск по коллекции; при совпадении у
// нескольких стикеров берётся тот, что дольше всех не отправлялся.
package character

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// StickerRef — разобранная ссылка: либо номер строки коллекции, либо эмодзи.
type StickerRef struct {
	ID    int64
	Emoji string
}

// parseStickerRef разбирает метку: сначала номер (он может стоять перед
// эмодзи: «#4 🐱»), затем первый эмодзи-кластер строки. Служебные заполнения
// и мусор — (нулевая ссылка, false).
func parseStickerRef(label string) (StickerRef, bool) {
	if stickerLabelNone(label) {
		return StickerRef{}, false
	}
	trimmed := strings.TrimSpace(label)
	if id, ok := ParseStickerLabel(trimmed); ok {
		return StickerRef{ID: id}, true
	}
	cluster, start, ok := firstEmojiCluster(trimmed)
	if !ok {
		return StickerRef{}, false
	}
	// Номер перед эмодзи («#4 🐱», «4 🐱»): модель пишет метку и поясняет её.
	if start > 0 {
		prefix := strings.TrimSpace(string([]rune(trimmed)[:start]))
		if id, ok := ParseStickerLabel(prefix); ok {
			return StickerRef{ID: id}, true
		}
	}
	return StickerRef{Emoji: cluster}, true
}

// stickerLabelNone — поле стикера заполнено «ничем»: пусто, null/none/nil,
// «нет», только пунктуация, «не используется в этом ходу». Это не запрос
// стикера и не ошибка модели — молча продолжаем ход.
func stickerLabelNone(label string) bool {
	s := strings.ToLower(strings.TrimSpace(label))
	if s == "" {
		return true
	}
	switch s {
	case "null", "none", "nil", "нет", "-", "—", "–", "n/a", "na", "undefined":
		return true
	}
	// Только пунктуация/пробелы и ни одного эмодзи — не запрос стикера.
	hasWord := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasWord = true
			break
		}
	}
	if !hasWord {
		if _, _, ok := firstEmojiCluster(s); !ok {
			return true
		}
	}
	for _, prefix := range []string{
		"не используется", "не нужен", "не нужна", "не нужно", "не требуется",
		"нет стикера", "не надо", "без стикера",
	} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// firstEmojiCluster — первый эмодзи строки с кластером целиком (ZWJ-цепочки,
// модификаторы тона, вариационные селекторы, keycap). start — индекс в рунах.
func firstEmojiCluster(s string) (cluster string, start int, ok bool) {
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if !isEmojiBase(runes[i]) && !isKeycapStart(runes, i) {
			continue
		}
		end := emojiClusterEnd(runes, i)
		return string(runes[i:end]), i, true
	}
	return "", 0, false
}

func isEmojiBase(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // эмоции, пиктограммы, флаги (кроме RI)
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // региональные индикаторы (флаги)
		return true
	case r >= 0x2600 && r <= 0x27BF: // разные символы и дингбаты
		return true
	case r >= 0x2B00 && r <= 0x2BFF:
		return true
	case r >= 0x2300 && r <= 0x23FF:
		return true
	case r >= 0x25A0 && r <= 0x25FF:
		return true
	case r >= 0x2900 && r <= 0x297F:
		return true
	case r == 0x00A9 || r == 0x00AE || r == 0x203C || r == 0x2049 ||
		r == 0x2122 || r == 0x2139 || r == 0x3030 || r == 0x303D ||
		r == 0x3297 || r == 0x3299:
		return true
	}
	return false
}

// isKeycapStart — «4️⃣»: цифра/#/* с вариационным селектором или keycap.
func isKeycapStart(runes []rune, i int) bool {
	if i >= len(runes) {
		return false
	}
	r := runes[i]
	if !(r >= '0' && r <= '9' || r == '#' || r == '*') {
		return false
	}
	return i+1 < len(runes) && (runes[i+1] == 0xFE0F || runes[i+1] == 0xFE0E || runes[i+1] == 0x20E3)
}

func emojiClusterEnd(runes []rune, i int) int {
	j := i + 1
	if isKeycapStart(runes, i) {
		for j < len(runes) && (runes[j] == 0xFE0F || runes[j] == 0xFE0E) {
			j++
		}
		if j < len(runes) && runes[j] == 0x20E3 {
			j++
		}
		return j
	}
	// Флаг — пара региональных индикаторов.
	if runes[i] >= 0x1F1E6 && runes[i] <= 0x1F1FF &&
		j < len(runes) && runes[j] >= 0x1F1E6 && runes[j] <= 0x1F1FF {
		j++
	}
	for j < len(runes) {
		r := runes[j]
		switch {
		case r == 0xFE0F || r == 0xFE0E: // вариационный селектор
			j++
		case r >= 0x1F3FB && r <= 0x1F3FF: // тон кожи
			j++
		case r >= 0xE0020 && r <= 0xE007F: // теги региональных флагов
			j++
		case r == 0x20E3: // enclosing keycap
			j++
		case r == 0x200D: // ZWJ: следующая эмодзи-база — часть кластера
			if j+1 < len(runes) && (isEmojiBase(runes[j+1]) || isKeycapStart(runes, j+1)) {
				j += 2
			} else {
				return j
			}
		default:
			return j
		}
	}
	return j
}

// normalizeStickerEmoji — вид эмодзи для сравнения: убираем вариационные
// селекторы и тон кожи, чтобы «❤️» и «❤», «👍🏻» и «👍» считались одним.
func normalizeStickerEmoji(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r == 0xFE0F || r == 0xFE0E || (r >= 0x1F3FB && r <= 0x1F3FF) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stickerColumnsByEmoji — квалифицированные колонки: запрос соединён с
// delivery_outbox, где тоже есть id.
const stickerColumnsByEmoji = `s.id, s.file_id, coalesce(s.file_unique_id, ''), s.emoji, s.set_name, s.note, s.source`

// ByEmoji — стикер коллекции по эмодзи. Если эмодзи совпадает у нескольких,
// берётся тот, что дольше всех не отправлялся (нет отправок — самый давний
// кандидат); ничья — меньший id. Стикера нет — (nil, nil), как у метки.
func (r *StickerRepo) ByEmoji(ctx context.Context, db DBTX, emoji string) (*Sticker, error) {
	want := normalizeStickerEmoji(emoji)
	if want == "" {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		SELECT `+stickerColumnsByEmoji+`,
		       coalesce(max(o.created_at), to_timestamp(0)) AS last_sent
		FROM stickers s
		LEFT JOIN delivery_outbox o
		  ON o.payload->>'kind' = 'sticker'
		 AND o.payload->>'file_id' = s.file_id
		GROUP BY s.id
		ORDER BY last_sent ASC, s.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("стикеры: коллекция по эмодзи не читается: %w", err)
	}
	defer rows.Close()
	var found *Sticker
	for rows.Next() {
		var s Sticker
		var lastSent time.Time
		if err := rows.Scan(&s.ID, &s.FileID, &s.FileUniqueID, &s.Emoji,
			&s.SetName, &s.Note, &s.Source, &lastSent); err != nil {
			return nil, fmt.Errorf("стикеры: строка коллекции по эмодзи не читается: %w", err)
		}
		if found == nil && normalizeStickerEmoji(s.Emoji) == want {
			row := s
			found = &row
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("стикеры: коллекция по эмодзи не дочитана: %w", err)
	}
	return found, nil
}
