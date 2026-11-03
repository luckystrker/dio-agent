// Коллекция стикеров персонажа (этап 25.3–25.5, решение 2026-11-03): стартовый
// набор — секция «Стикеры» persona-файла (источник persona, синхронизируется
// при старте), присланные и понравившиеся стикеры персонаж сохраняет сам
// (источник saved). Модель выбирает стикер меткой из панели (#id) — file_id
// подставляет runtime, модель его не видит и не придумывает (§1 п.7).
package character

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// Источники строки коллекции.
const (
	StickerSourcePersona = "persona"
	StickerSourceSaved   = "saved"
)

// Потолки панели и полей.
const (
	// stickerPanelItems — сколько стикеров коллекции видно модели в панели:
	// список — подсказка для выбора, а не каталог.
	stickerPanelItems = 12
	// stickerPanelMaxChars — объём самого списка (без заголовка).
	stickerPanelMaxChars = 700
	// stickerPanelHeaderOverhead — запас заголовка списка в панели.
	stickerPanelHeaderOverhead = 120
	// stickerNotePanelLimit — заметка в панели режется: смысл «когда уместен»
	// умещается в строку.
	stickerNotePanelLimit = 80
	// stickerNoteLimit — потолок заметки, которую предлагает модель (25.5).
	stickerNoteLimit = 200
	// maxStickerLabel — потолок метки в TurnResult.sticker.
	maxStickerLabel = 32
)

// Sticker — строка коллекции.
type Sticker struct {
	ID           int64
	FileID       string
	FileUniqueID string
	Emoji        string
	SetName      string
	Note         string
	Source       string
}

// SourceSticker — строка секции «Стикеры» persona-файла.
type SourceSticker struct {
	FileID string
	Emoji  string
	Note   string
}

// StickerRepo — доступ к таблице stickers (миграция 000015).
type StickerRepo struct{}

// NewStickerRepo создаёт репозиторий (без состояния).
func NewStickerRepo() *StickerRepo { return &StickerRepo{} }

// stickerColumns — порядок колонок выборки (List/ByLabel/Save).
const stickerColumns = `id, file_id, coalesce(file_unique_id, ''), emoji, set_name, note, source`

func scanSticker(row interface{ Scan(...any) error }) (Sticker, error) {
	var s Sticker
	if err := row.Scan(&s.ID, &s.FileID, &s.FileUniqueID, &s.Emoji, &s.SetName, &s.Note, &s.Source); err != nil {
		return Sticker{}, err
	}
	return s, nil
}

// SyncPersona приводит строки персоны к файлу-источнику истины (§7.1):
// отсутствующие в файле строки персоны удаляются, остальные обновляются по
// file_id; сохранённые персонажем стикеры (saved) не трогаются. Возвращает
// число строк набора персоны.
func (r *StickerRepo) SyncPersona(ctx context.Context, db DBTX, entries []SourceSticker) (int, error) {
	// Простая последовательность запросов: набор персоны мал (десятки строк),
	// а параллельные писатели в коллекцию — только этот старт и ходы.
	fileIDs := []string{} // пустой массив, не NULL: NOT (file_id = ANY(NULL)) не удаляет ничего
	for _, e := range entries {
		fileID := strings.TrimSpace(e.FileID)
		if fileID == "" {
			continue
		}
		fileIDs = append(fileIDs, fileID)
		if _, err := db.Exec(ctx, `
			INSERT INTO stickers (file_id, emoji, note, source)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (file_id) WHERE source = 'persona'
			DO UPDATE SET emoji = EXCLUDED.emoji, note = EXCLUDED.note`,
			fileID, strings.TrimSpace(e.Emoji), strings.TrimSpace(e.Note), StickerSourcePersona); err != nil {
			return 0, fmt.Errorf("стикеры: строка персоны не сохранена: %w", err)
		}
	}
	// Строки персоны, которых больше нет в файле, удаляются: файл — источник.
	_, err := db.Exec(ctx, `
		DELETE FROM stickers
		WHERE source = 'persona' AND NOT (file_id = ANY($1::text[]))`,
		fileIDs)
	if err != nil {
		return 0, fmt.Errorf("стикеры: устаревшие строки персоны не удалены: %w", err)
	}
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM stickers WHERE source = 'persona'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("стикеры: набор персоны не посчитан: %w", err)
	}
	return n, nil
}

// List — коллекция для панели: свежие первыми (недавно сохранённые видны),
// с потолком числа строк.
func (r *StickerRepo) List(ctx context.Context, db DBTX, limit int) ([]Sticker, error) {
	if limit <= 0 {
		limit = stickerPanelItems
	}
	rows, err := db.Query(ctx,
		`SELECT `+stickerColumns+` FROM stickers ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("стикеры: коллекция не читается: %w", err)
	}
	defer rows.Close()
	var out []Sticker
	for rows.Next() {
		s, err := scanSticker(rows)
		if err != nil {
			return nil, fmt.Errorf("стикеры: строка коллекции не читается: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("стикеры: коллекция не дочитана: %w", err)
	}
	return out, nil
}

// ByLabel — стикер по метке из TurnResult (#id). Нет такого — (nil, nil):
// метка не из панели мягко обнуляется, как мусорная реакция при answer.
func (r *StickerRepo) ByLabel(ctx context.Context, db DBTX, label string) (*Sticker, error) {
	id, ok := ParseStickerLabel(label)
	if !ok {
		return nil, nil
	}
	s, err := scanSticker(db.QueryRow(ctx,
		`SELECT `+stickerColumns+` FROM stickers WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("стикеры: метка %s не прочитана: %w", label, err)
	}
	return &s, nil
}

// Save добавляет присланный стикер в коллекцию (25.5). Дубли отсекаются по
// file_unique_id; стикер персоны узнаётся по file_id и получает недостающий
// уникальный id, а не вторую строку. Возвращает id и признак «создан».
func (r *StickerRepo) Save(ctx context.Context, db DBTX, info telegram.StickerInfo, note string) (int64, bool, error) {
	fileID := strings.TrimSpace(info.FileID)
	if fileID == "" {
		return 0, false, nil
	}
	unique := strings.TrimSpace(info.FileUniqueID)
	if unique != "" {
		// Строка персоны того же file_id: дорисовываем уникальный id (без него
		// дедупликация «сохраню себе» не работала бы до ручной правки).
		if _, err := db.Exec(ctx, `
			UPDATE stickers SET file_unique_id = $2
			WHERE file_id = $1 AND file_unique_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM stickers WHERE file_unique_id = $2)`,
			fileID, unique); err != nil {
			return 0, false, fmt.Errorf("стикеры: уникальный id не дорисован: %w", err)
		}
	}
	// Без уникального id стикер может быть уже известен по file_id (строка
	// персоны до встречи): вторую строку не создаём.
	if unique == "" {
		var existing int64
		err := db.QueryRow(ctx, `SELECT id FROM stickers WHERE file_id = $1 LIMIT 1`, fileID).Scan(&existing)
		if err == nil {
			return existing, false, nil
		}
		if !isNoRows(err) {
			return 0, false, fmt.Errorf("стикеры: стикер по file_id не найден: %w", err)
		}
	}
	var id int64
	err := db.QueryRow(ctx, `
		INSERT INTO stickers (file_id, file_unique_id, emoji, set_name, note, source)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6)
		ON CONFLICT (file_unique_id) WHERE file_unique_id IS NOT NULL DO NOTHING
		RETURNING id`,
		fileID, unique, strings.TrimSpace(info.Emoji), strings.TrimSpace(info.SetName),
		strings.TrimSpace(note), StickerSourceSaved).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !isNoRows(err) {
		return 0, false, fmt.Errorf("стикеры: стикер не сохранён: %w", err)
	}
	// Дубль: строка уже есть — находим её, чтобы отличить «уже своё» от сбоя.
	query := `SELECT id FROM stickers WHERE file_id = $1`
	arg := fileID
	if unique != "" {
		query = `SELECT id FROM stickers WHERE file_unique_id = $1`
		arg = unique
	}
	if serr := db.QueryRow(ctx, query, arg).Scan(&id); serr != nil {
		return 0, false, fmt.Errorf("стикеры: дубль не перечитан: %w", serr)
	}
	return id, false, nil
}

// ParseStickerLabel — метка коллекции «#12» (или «12») → id. Мусор, отрицательные
// и переполняющие числа отвергаются: модель не может сослаться на несуществующее.
func ParseStickerLabel(label string) (int64, bool) {
	trimmed := strings.TrimSpace(label)
	trimmed = strings.TrimPrefix(trimmed, "#")
	if trimmed == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// stickerLabel — метка стикера для панели и TurnResult.sticker.
func stickerLabel(id int64) string { return "#" + strconv.FormatInt(id, 10) }

// RenderStickerList — строки панели состояния: метка, эмодзи и заметка.
// Заголовок прямо говорит, каким полем отправлять. Пустая коллекция — пустая
// строка (панель не разрастается служебным шумом).
func RenderStickerList(stickers []Sticker) string {
	if len(stickers) == 0 {
		return ""
	}
	lines := []string{"твои стикеры (отправить — поле sticker, метка):"}
	total := 0
	for _, s := range stickers {
		line := stickerLabel(s.ID)
		if emoji := sanitizePanelValue(s.Emoji, 8); emoji != "" {
			line += " " + emoji
		}
		if note := sanitizePanelValue(s.Note, stickerNotePanelLimit); note != "" {
			line += " — " + note
		}
		if total+len([]rune(line)) > stickerPanelMaxChars {
			break
		}
		total += len([]rune(line))
		lines = append(lines, line)
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}
