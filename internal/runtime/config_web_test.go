package runtime

import (
	"strings"
	"testing"

	"github.com/luckystrker/dio-agent/internal/web"
)

// TestParseWebSettings — правила группы WEB_*: по умолчанию выключено,
// ключ уместен только у ключевых провайдеров, неизвестный провайдер — ошибка.
func TestParseWebSettings(t *testing.T) {
	env := func(vals map[string]string) Getenv {
		return func(key string) (string, bool) {
			v, ok := vals[key]
			return v, ok
		}
	}

	if w, err := parseWebSettings(env(nil)); err != nil || w.Provider != web.ProviderOff {
		t.Fatalf("по умолчанию веб выключен: %+v %v", w, err)
	}
	if _, err := parseWebSettings(env(map[string]string{"WEB_SEARCH_API_KEY": "k"})); err == nil ||
		!strings.Contains(err.Error(), "WEB_SEARCH_PROVIDER") {
		t.Fatalf("ключ без провайдера должен быть ошибкой: %v", err)
	}
	if _, err := parseWebSettings(env(map[string]string{"WEB_SEARCH_PROVIDER": "tavily"})); err == nil {
		t.Fatal("tavily без ключа должен быть ошибкой")
	}
	w, err := parseWebSettings(env(map[string]string{
		"WEB_SEARCH_PROVIDER": "tavily", "WEB_SEARCH_API_KEY": "k",
		"WEB_MAX_SEARCH_PER_TASK": "1", "WEB_MAX_FETCH_PER_TASK": "7",
	}))
	if err != nil {
		t.Fatalf("tavily с ключом: %v", err)
	}
	if w.Provider != "tavily" || w.APIKey != "k" || w.MaxSearchPerTask != 1 || w.MaxFetchPerTask != 7 {
		t.Fatalf("поля не прочитаны: %+v", w)
	}
	if _, err := parseWebSettings(env(map[string]string{"WEB_SEARCH_PROVIDER": "bogus"})); err == nil {
		t.Fatal("неизвестный провайдер должен быть ошибкой")
	}
}
