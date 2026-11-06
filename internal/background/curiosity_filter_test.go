// Юнит-тесты фильтра тем-сидов поиска интереса (этап 38.8, порог уточнён
// этапом 41.2 M10): живой прогон 2026-11-06 создал explore «по теме «как сам»» —
// мусорная тема из памяти, 9 модельных вызовов и 78 705 токенов на пустую
// работу. Тема годна, если это не приветствие/вопрос-формула и она
// содержательна: ≥2 слов или ≥3 букв («море», «лес» — годные интересы).
package background

import (
	"math/rand"
	"testing"
)

func TestValidCuriosityTheme(t *testing.T) {
	cases := []struct {
		theme string
		want  bool
	}{
		{"портовые краны", true},        // два слова
		{"виниловые пластинки", true},   // два слова
		{"полировка", true},             // одно слово, 9 букв
		{"нейросети", true},             // одно слово, 9 букв
		{"море", true},                  // одно слово, 4 буквы — годный интерес (41.2 M10)
		{"лес", true},                   // 3 буквы + не приветствие
		{"джаз", true},                  // 4 буквы
		{"хм", false},                   // слишком коротко
		{"да", false},                   // слишком коротко
		{"привет", false},               // приветствие
		{"Привет!", false},              // приветствие с пунктуацией
		{"как сам", false},              // вопрос-формула (боевой мусор)
		{"как сам?", false},             // вопрос-формула с «?»
		{"чё как", false},               // вопрос-формула
		{"как дела", false},             // вопрос-формула
		{"что делаешь", false},          // вопрос-формула
		{"как работает память?", false}, // вопрос, а не тема
		{"]", false},                    // пунктуация
		{"...", false},                  // пунктуация
		{"", false},                     // пусто
		{"   ", false},                  // пробелы
	}
	for _, tc := range cases {
		if got := validCuriosityTheme(tc.theme); got != tc.want {
			t.Errorf("validCuriosityTheme(%q) = %v, хочу %v", tc.theme, got, tc.want)
		}
	}
}

// TestPickCuriosityThemeFiltersGarbage — мусор не выбирается даже когда это
// единственный «непустой» источник: нет годной темы — пусто (скука промолчит).
func TestPickCuriosityThemeFiltersGarbage(t *testing.T) {
	rnd := rand.New(rand.NewSource(3))
	if got := PickCuriosityTheme(rnd, []string{"как сам", "привет"}, []string{"]"}, nil); got != "" {
		t.Fatalf("выбрана мусорная тема %q", got)
	}
	for i := 0; i < 50; i++ {
		got := PickCuriosityTheme(rand.New(rand.NewSource(int64(i))), []string{"как сам", "полировка"}, []string{"чё как"})
		if got != "полировка" {
			t.Fatalf("из мусора и годной темы выбрано %q, хочу «полировка»", got)
		}
	}
}

// TestValidCuriosityThemeShortWord — M10: короткий однословный интерес
// («море», «лес») — годная тема для поиска от скуки; приветствия и вопросы — нет.
func TestValidCuriosityThemeShortWord(t *testing.T) {
	for _, theme := range []string{"море", "лес", "космос", "джаз"} {
		if !validCuriosityTheme(theme) {
			t.Fatalf("годная однословная тема %q отброшена", theme)
		}
	}
	for _, theme := range []string{"привет", "как сам", "что делаешь", "чё как", "да", "??", "как", "что"} {
		if validCuriosityTheme(theme) {
			t.Fatalf("мусор %q принят темой", theme)
		}
	}
	if got := PickCuriosityTheme(nil, []string{"море"}); got != "море" {
		t.Fatalf("PickCuriosityTheme выбрал %q, хочу «море»", got)
	}
}
