// Юнит-тесты подкоманды replay (40.2): вердикт TurnResult с правилами брака
// 40.4, разбор флагов и копия запроса — без сети, БД и Telegram.
package main

import (
	"strings"
	"testing"
)

// TestReplayVerdict — пустой content, невалидный JSON, брак в СЫРЫХ messages
// (до нормализации бота) и здоровый ответ различаются. Replay — инструмент
// отладки: он обязан оценивать сырой ответ модели, даже если ParseTurnResult
// уже срезал служебные метки (этап 41, m10).
func TestReplayVerdict(t *testing.T) {
	cases := []struct {
		name    string
		content string
		bad     bool
		want    string // подстрока вердикта
	}{
		{"пусто", "   ", true, "пустой content"},
		{"не JSON", "привет", true, "невалидный TurnResult"},
		{"брак-пунктуация", `{"decision":"answer","messages":[","]}`, true, "брак"},
		{"брак-поле схемы", `{"decision":"answer","messages":["mood_delta},"]}`, true, "mood_delta"},
		{"брак-метка в тексте", `{"decision":"answer","messages":["привет #m12"]}`, true, "#m"},
		{"брак-метка в начале", `{"decision":"answer","messages":["#m1034 привет"]}`, true, "после нормализации"},
		{"здоровый", `{"decision":"answer","messages":["привет, как ты?"]}`, false, "валиден"},
		{"ignore", `{"decision":"ignore"}`, false, "валиден"},
		{"код не брак", `{"decision":"answer","messages":["func main() {"]}`, false, "валиден"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, bad := replayVerdict(tc.content)
			if bad != tc.bad {
				t.Fatalf("bad = %v, хочу %v (вердикт: %s)", bad, tc.bad, verdict)
			}
			if !strings.Contains(verdict, tc.want) {
				t.Fatalf("вердикт %q не содержит %q", verdict, tc.want)
			}
		})
	}
}

// TestVariantListSet — --variant понимает запятые, повторы и пустые куски.
func TestVariantListSet(t *testing.T) {
	var v variantList
	for _, raw := range []string{"no-labels, no-panel", "", "history-last-3", " , "} {
		if err := v.Set(raw); err != nil {
			t.Fatalf("Set(%q): %v", raw, err)
		}
	}
	want := []string{"no-labels", "no-panel", "history-last-3"}
	if len(v) != len(want) {
		t.Fatalf("варианты = %v, хочу %v", v, want)
	}
	for i := range want {
		if v[i] != want[i] {
			t.Fatalf("варианты = %v, хочу %v", v, want)
		}
	}
	if got := v.String(); got != "no-labels,no-panel,history-last-3" {
		t.Fatalf("String() = %q", got)
	}
}

// TestCloneRequestRejectsNonObject — request дампа обязан быть объектом:
// массив/строка дают явную ошибку, а не пустую map.
func TestCloneRequestRejectsNonObject(t *testing.T) {
	for _, raw := range []string{`[]`, `"строка"`, `42`} {
		if _, err := cloneRequest([]byte(raw)); err == nil {
			t.Fatalf("cloneRequest(%s) не вернул ошибку", raw)
		}
	}
	body, err := cloneRequest([]byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("объект не разобран: %v", err)
	}
	if body["model"] != "m" {
		t.Fatalf("копия запроса: %v", body)
	}
}

// TestPrintableTruncates — печать content не вываливает гигантскую простыню,
// но короткий текст не трогает.
func TestPrintableTruncates(t *testing.T) {
	if got := printable("ok"); got != "ok" {
		t.Fatalf("короткий текст изменён: %q", got)
	}
	long := strings.Repeat("я", 9000)
	got := printable(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("длинный текст не обрезан: %d символов", len([]rune(got)))
	}
	if len([]rune(got)) != 8001 {
		t.Fatalf("обрезано до %d рун, хочу 8001 (8000 + …)", len([]rune(got)))
	}
}

// TestReplayUsageMentionsAblations — справка перечисляет все абляции: человек
// не должен угадывать имена.
func TestReplayUsageMentionsAblations(t *testing.T) {
	for _, want := range []string{
		"no-labels", "no-timestamps", "no-panel", "history-last-N",
		"assistant-as-json", "merge-assistant", "no-tools", "temperature=X", "--n",
	} {
		if !strings.Contains(replayUsage, want) {
			t.Fatalf("в справке нет %q", want)
		}
	}
}
