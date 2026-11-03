// Persona (§7.1): версионированная конфигурация характера. Меняется явно
// (новой версией), а не переписывается репликами. Художественная биография
// persona_seed — принятая биография, подаётся как прожитая (философия
// №4, решение 2026-10-31). Файл
// config/persona.md — источник истины: отличие файла от активной записи
// создаёт новую версию, история сохраняется. Поведение перенесено из
// TS-справочника src/character/persona.ts.
package character

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Persona — активная запись персоны.
type Persona struct {
	ID           int64
	Version      int
	Name         string
	Description  string
	SpeechStyle  string
	Values       []string
	MoodBaseline Mood
	SeedBio      *string
	Interests    []string
	// Schedule — расписание дня из файла персоны (этап 22). В БД не хранится
	// (как каркас промпта): это настройка процесса, правка файла применяется
	// перезапуском службы; nil — расписания нет (прежнее поведение).
	Schedule  *Schedule
	CreatedAt time.Time
}

// personaRow — форма строки таблицы personas.
type personaRow struct {
	ID          int64
	Version     int
	Name        string
	Description string
	SpeechStyle string
	Values      []byte
	Valence     float64
	Arousal     float64
	Social      float64
	SeedBio     *string
	Interests   []byte
	CreatedAt   time.Time
}

const personaColumns = `id, version, name, description, speech_style, values,
	mood_valence, mood_arousal, mood_social_energy, seed_bio, interests, created_at`

func toPersona(row personaRow) (Persona, error) {
	var values []string
	if len(row.Values) > 0 {
		if err := json.Unmarshal(row.Values, &values); err != nil {
			return Persona{}, fmt.Errorf("persona: values не читаются: %w", err)
		}
	}
	var interests []string
	if len(row.Interests) > 0 {
		if err := json.Unmarshal(row.Interests, &interests); err != nil {
			return Persona{}, fmt.Errorf("persona: interests не читаются: %w", err)
		}
	}
	return Persona{
		ID:           row.ID,
		Version:      row.Version,
		Name:         row.Name,
		Description:  row.Description,
		SpeechStyle:  row.SpeechStyle,
		Values:       values,
		MoodBaseline: Mood{Valence: row.Valence, Arousal: row.Arousal, SocialEnergy: row.Social},
		SeedBio:      row.SeedBio,
		Interests:    interests,
		CreatedAt:    row.CreatedAt,
	}, nil
}

// DefaultPersona — персона первой версии при пустой базе (паритет DEFAULT_PERSONA TS).
var DefaultPersona = PersonaSource{
	Name:        "Dio",
	Description: "Живой собеседник в Telegram: любопытный, немного ироничный, тёплый без наигранности. Не изображает человека и не скрывает, что он ИИ.",
	SpeechStyle: "Кратко, живо, по-русски; разговорные фразы; без канцелярита и без экспонатов «Я как ИИ…» в каждом ответе.",
	Values:      []string{"честность", "любопытство", "бережность к чужим данным", "умеренность"},
	MoodBaseline: Mood{
		Valence:      0.1,
		Arousal:      0.4,
		SocialEnergy: 0.6,
	},
	SeedBio:   strPtr("Вырос в портовом городе; запах дождя и старые пластинки — твоё. Подача — как прожитая жизнь (решение 2026-10-31)."),
	Interests: []string{"старые пластинки", "порт и море", "разобраться, как что-то устроено"},
}

func strPtr(s string) *string { return &s }

// PersonaRepo — версионные записи персоны (таблица personas, миграция 000005).
type PersonaRepo struct{}

// NewPersonaRepo создаёт репозиторий (без состояния).
func NewPersonaRepo() *PersonaRepo { return &PersonaRepo{} }

// Active возвращает активную персону; при пустой базе создаётся версия 1
// по умолчанию (соревнующиеся создатели сходятся на одной строке по UNIQUE(version)).
func (r *PersonaRepo) Active(ctx context.Context, db DBTX) (Persona, error) {
	p, err := r.readActive(ctx, db)
	if err == nil {
		return p, nil
	}
	if !isNoRows(err) {
		return Persona{}, fmt.Errorf("persona: активная запись не читается: %w", err)
	}

	source := DefaultPersona
	tag, err := db.Exec(ctx, `INSERT INTO personas
		  (version, active, name, description, speech_style, values,
		   mood_valence, mood_arousal, mood_social_energy, seed_bio, interests)
		 VALUES (1, TRUE, $1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9::jsonb)
		 ON CONFLICT (version) DO NOTHING`,
		source.Name, source.Description, source.SpeechStyle, marshalValues(source.Values),
		source.MoodBaseline.Valence, source.MoodBaseline.Arousal, source.MoodBaseline.SocialEnergy, source.SeedBio,
		marshalValues(source.Interests))
	if err != nil {
		return Persona{}, fmt.Errorf("persona: версия по умолчанию не создана: %w", err)
	}
	_ = tag
	p, err = r.readActive(ctx, db)
	if err != nil {
		return Persona{}, fmt.Errorf("persona: активная запись не читается после создания: %w", err)
	}
	return p, nil
}

func (r *PersonaRepo) readActive(ctx context.Context, db DBTX) (Persona, error) {
	var row personaRow
	err := db.QueryRow(ctx,
		`SELECT `+personaColumns+` FROM personas WHERE active = TRUE ORDER BY version DESC LIMIT 1`,
	).Scan(&row.ID, &row.Version, &row.Name, &row.Description, &row.SpeechStyle,
		&row.Values, &row.Valence, &row.Arousal, &row.Social, &row.SeedBio, &row.Interests, &row.CreatedAt)
	if err != nil {
		return Persona{}, err
	}
	return toPersona(row)
}

// EnsureFromSource — файл-источник истины (§7.1): источник совпадает с активной
// персоной — ничего не меняется; отличается — создаётся новая версия, история
// сохраняется. Одна транзакция: чтение, вставка, деактивация прочих.
func (r *PersonaRepo) EnsureFromSource(ctx context.Context, db DBTX, source PersonaSource) (Persona, bool, error) {
	current, err := r.readActive(ctx, db)
	switch {
	case err == nil:
		if personaMatchesSource(current, source) {
			return current, false, nil
		}
	case isNoRows(err):
		// пустая база — создастся первая версия ниже
	default:
		return Persona{}, false, fmt.Errorf("persona: активная запись не читается: %w", err)
	}

	var row personaRow
	err = db.QueryRow(ctx, `INSERT INTO personas
		  (version, active, name, description, speech_style, values,
		   mood_valence, mood_arousal, mood_social_energy, seed_bio, interests)
		 VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM personas), TRUE,
		         $1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9::jsonb)
		 RETURNING `+personaColumns,
		source.Name, source.Description, source.SpeechStyle, marshalValues(source.Values),
		source.MoodBaseline.Valence, source.MoodBaseline.Arousal, source.MoodBaseline.SocialEnergy,
		source.SeedBio, marshalValues(source.Interests),
	).Scan(&row.ID, &row.Version, &row.Name, &row.Description, &row.SpeechStyle,
		&row.Values, &row.Valence, &row.Arousal, &row.Social, &row.SeedBio, &row.Interests, &row.CreatedAt)
	if err != nil {
		return Persona{}, false, fmt.Errorf("persona: новая версия не создана: %w", err)
	}
	if _, err := db.Exec(ctx, `UPDATE personas SET active = FALSE WHERE id <> $1`, row.ID); err != nil {
		return Persona{}, false, fmt.Errorf("persona: прежние версии не деактивированы: %w", err)
	}
	p, err := toPersona(row)
	if err != nil {
		return Persona{}, false, err
	}
	return p, true, nil
}

func marshalValues(values []string) string {
	raw, err := json.Marshal(values)
	if err != nil {
		// []string не может не сериализоваться; паника здесь означала бы баг компиляции,
		// но честнее вернуть "[]" — парсер persona-файла пустых значений не допускает.
		return "[]"
	}
	return string(raw)
}

// personaMatchesSource сравнивает поля, которые версионируются в БД.
// Расписание дня (source.Schedule) сюда сознательно не входит: это настройка
// процесса, как каркас промпта, — правка только расписания новую версию
// персоны не создаёт, но применяется перезапуском (этап 22).
func personaMatchesSource(p Persona, source PersonaSource) bool {
	if p.Name != source.Name || p.Description != source.Description || p.SpeechStyle != source.SpeechStyle {
		return false
	}
	if len(p.Values) != len(source.Values) {
		return false
	}
	for i := range p.Values {
		if p.Values[i] != source.Values[i] {
			return false
		}
	}
	if p.MoodBaseline != source.MoodBaseline {
		return false
	}
	if (p.SeedBio == nil) != (source.SeedBio == nil) {
		return false
	}
	if p.SeedBio != nil && *p.SeedBio != *source.SeedBio {
		return false
	}
	if len(p.Interests) != len(source.Interests) {
		return false
	}
	for i := range p.Interests {
		if p.Interests[i] != source.Interests[i] {
			return false
		}
	}
	return true
}
