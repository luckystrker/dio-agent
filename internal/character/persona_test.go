// Юнит-тесты чтения строки персоны (без БД): ветки разбора JSON-колонок.
package character

import (
	"strings"
	"testing"
)

// TestToPersonaBrokenInterests — 29.4: битый JSON в колонке interests не
// проглатывается молча (иначе персона молча остаётся без интересов и фон
// выбирает тему из пустоты) и не роняет процесс — возвращается ошибка с
// понятным именем колонки.
func TestToPersonaBrokenInterests(t *testing.T) {
	for _, raw := range []string{`{"не":"массив"}`, `не json`, `["море",`} {
		_, err := toPersona(personaRow{ID: 1, Interests: []byte(raw)})
		if err == nil {
			t.Fatalf("битый interests %q прочитан без ошибки", raw)
		}
		if !strings.Contains(err.Error(), "interests") {
			t.Fatalf("ошибка не называет колонку interests: %v", err)
		}
	}

	// Валидный массив и пустое значение читаются как раньше.
	p, err := toPersona(personaRow{ID: 2, Interests: []byte(`["море","пластинки"]`)})
	if err != nil {
		t.Fatalf("валидные интересы не прочитаны: %v", err)
	}
	if len(p.Interests) != 2 || p.Interests[0] != "море" {
		t.Fatalf("интересы прочитаны неверно: %v", p.Interests)
	}
	if p, err := toPersona(personaRow{ID: 3}); err != nil || len(p.Interests) != 0 {
		t.Fatalf("пустые интересы: %v, err=%v", p.Interests, err)
	}
}
