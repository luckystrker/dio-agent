//go:build db

// БД-тест источника тем для скуки (этап 38.8): темы берутся только из
// содержательной памяти (claim/thought/preference), а не из «прожитого» —
// реплики разговора давали мусор «как сам», и explore уходил искать эту тему.
package background

import (
	"context"
	"fmt"
	"testing"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// seedThemedMemory — запись памяти с сущностью-темой (свежая по recorded_at).
func seedThemedMemory(t *testing.T, ctx context.Context, f *fixture, kind, text, entityName string) {
	t.Helper()
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, f.pool,
		memory.SourceRuntimeAction, fmt.Sprintf("test:theme:%s:%s", kind, entityName), []string{bgScope})
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	source := memory.SourceRef{Kind: memory.SourceRuntimeAction, Ref: fmt.Sprintf("test:theme:%s:%s", kind, entityName)}
	item, err := (memory.Items{}).Insert(ctx, f.pool, memory.NewItem{
		Kind:         kind,
		Text:         text,
		Salience:     0.5,
		ProvenanceID: provenanceID,
		Source:       &source,
		Links:        []memory.EntityLink{{Name: entityName, Kind: "topic", Role: "subject"}},
	}, bgScope)
	if err != nil {
		t.Fatalf("запись памяти не вставлена: %v", err)
	}
	_ = item
}

func TestRecentMemoryThemesSkipsReplies(t *testing.T) {
	f, ctx := newFixture(t)

	// Реплика разговора (experience) с «темой» «как сам» — боевой мусор.
	seedThemedMemory(t, ctx, f, memory.KindExperience, "«как сам» — так спросил Коля", "как сам")
	// Содержательная память: мысль и предпочтение.
	seedThemedMemory(t, ctx, f, memory.KindThought, "портовые краны — интересная тема", "портовые краны")
	seedThemedMemory(t, ctx, f, memory.KindPreference, "любит старые пластинки", "виниловые пластинки")

	themes, err := f.sched.recentMemoryThemes(ctx, bgScope)
	if err != nil {
		t.Fatalf("темы памяти не прочитаны: %v", err)
	}
	for _, theme := range themes {
		if theme == "как сам" {
			t.Fatalf("тема из реплики попала в источники скуки: %v", themes)
		}
	}
	want := map[string]bool{"портовые краны": true, "виниловые пластинки": true}
	for _, theme := range themes {
		delete(want, theme)
	}
	if len(want) > 0 {
		t.Fatalf("потеряны темы из claim/thought/preference: %v (получено %v)", want, themes)
	}
}
