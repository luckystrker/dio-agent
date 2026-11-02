// Юнит-тесты вспомогательных функций item.go (без БД): сборка tsquery.
package memory

import "testing"

// TestOrTsQueryHyphenIsSeparator — 17.13: дефис в классе токена превращал
// «арка-фанера» в одну лексему to_tsquery с фразовой семантикой — запись
// «арка из фанеры» не находилась (живая БД, эвалы 2026-11-02). Дефис —
// разделитель: два токена с OR-семантикой.
func TestOrTsQueryHyphenIsSeparator(t *testing.T) {
	cases := map[string]string{
		"арка-фанера":     "арка | фанера",
		"что-то":          "что | то",
		"санкт-петербург": "санкт | петербург",
	}
	for query, want := range cases {
		if got := orTsQuery(query); got != want {
			t.Errorf("orTsQuery(%q) = %q, хочу %q", query, got, want)
		}
	}

	// Обычные запросы не регрессируют: слова и пунктуация как раньше.
	if got := orTsQuery("коллекционирует клавиатуры, грибы!"); got != "коллекционирует | клавиатуры | грибы" {
		t.Errorf("обычный запрос изменился: %q", got)
	}
	if got := orTsQuery("  "); got != "" {
		t.Errorf("пустой запрос дал %q, хочу пусто", got)
	}
}
