// Источники и provenance (спека §8.3, §9.1, §18). Ссылки на источники
// назначает и проверяет runtime; модель не может сослаться на несуществующее
// посещение сайта. Provenance хранит разрешённые аудитории производного:
// пересечение скоупов источников считается при записи; чтение перепроверяет
// (ItemVisible) — missing/invalid → internal.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
)

// Виды источников/provenance (§18): journal event, web document, runtime
// action, версия persona.
const (
	SourceJournalEvent   = "journal_event"
	SourceWebDocument    = "web_document"
	SourceRuntimeAction  = "runtime_action"
	SourcePersonaVersion = "persona_version"
)

var sourceKinds = map[string]bool{
	SourceJournalEvent: true, SourceWebDocument: true,
	SourceRuntimeAction: true, SourcePersonaVersion: true,
}

// SourceRef — стабильная ссылка на источник для записи.
type SourceRef struct {
	Kind     string
	Ref      string
	Metadata map[string]any
}

// Validate проверяет вид и непустоту ссылки.
func (s SourceRef) Validate() error {
	if !sourceKinds[s.Kind] {
		return fmt.Errorf("%w: неизвестный вид источника %q", ErrInvalidCandidate, s.Kind)
	}
	if s.Ref == "" {
		return fmt.Errorf("%w: пустая ссылка источника", ErrInvalidCandidate)
	}
	return nil
}

// SourceRepo — sources и provenance.
type SourceRepo struct{}

// EnsureSource создаёт или переиспользует источник по (kind, ref); возвращает
// id (уникальный индекс sources_kind_ref_idx делает операцию гонко-безопасной).
func (SourceRepo) EnsureSource(ctx context.Context, db DBTX, source SourceRef) (int64, error) {
	if err := source.Validate(); err != nil {
		return 0, err
	}
	metadata := source.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return 0, fmt.Errorf("sources: сериализация metadata: %w", err)
	}

	if _, err := db.Exec(ctx,
		`INSERT INTO sources (kind, ref, metadata) VALUES ($1, $2, $3)
		 ON CONFLICT (kind, ref) DO NOTHING`,
		source.Kind, source.Ref, raw); err != nil {
		return 0, fmt.Errorf("sources: вставка: %w", err)
	}
	var id int64
	if err := db.QueryRow(ctx,
		"SELECT id FROM sources WHERE kind = $1 AND ref = $2",
		source.Kind, source.Ref).Scan(&id); err != nil {
		return 0, fmt.Errorf("sources: перечитывание: %w", err)
	}
	return id, nil
}

// EnsureProvenance создаёт или переиспользует разрешение по (kind, ref).
// Скоупы валидируются по §9.1: разрешены public, internal, chat:<id>;
// пустой список допустим — он означает «пересечение источников не дало
// общего чата» → производное внутреннее (§9.1). Невалидный элемент —
// явная ошибка записи: в хранилище не должно попадать битых разрешений,
// а чтение на всякий случай трактует их как internal (ItemVisible).
func (SourceRepo) EnsureProvenance(ctx context.Context, db DBTX, kind, ref string, scopes []string) (int64, error) {
	if !sourceKinds[kind] {
		return 0, fmt.Errorf("%w: неизвестный вид provenance %q", ErrInvalidCandidate, kind)
	}
	if ref == "" {
		return 0, fmt.Errorf("%w: пустая ссылка provenance", ErrInvalidCandidate)
	}
	for _, s := range scopes {
		if !IsValidScope(s) {
			return 0, fmt.Errorf("%w: %q", ErrInvalidScope, s)
		}
	}

	var existing int64
	err := db.QueryRow(ctx,
		"SELECT id FROM provenance WHERE kind = $1 AND ref = $2 ORDER BY id LIMIT 1",
		kind, ref).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !isNoRows(err) {
		return 0, fmt.Errorf("provenance: поиск: %w", err)
	}

	var id int64
	if err := db.QueryRow(ctx,
		`INSERT INTO provenance (kind, ref, scopes) VALUES ($1, $2, $3) RETURNING id`,
		kind, ref, scopes).Scan(&id); err != nil {
		return 0, fmt.Errorf("provenance: вставка: %w", err)
	}
	return id, nil
}
