// Тесты поля image_url контракта TurnResult (этап 36.2): только полный
// http(s)-адрес; при решениях без доставки текста поле мягко обнуляется.
package character

import (
	"strings"
	"testing"
)

func TestParseTurnResultImageURL(t *testing.T) {
	ok, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["вот"],"image_url":"https://cdn.example.com/a.jpg"}`))
	if err != nil {
		t.Fatalf("валидный image_url отклонён: %v", err)
	}
	if ok.ImageURL != "https://cdn.example.com/a.jpg" {
		t.Fatalf("image_url = %q", ok.ImageURL)
	}

	for _, bad := range []string{
		`{"decision":"answer","messages":["x"],"image_url":"javascript:alert(1)"}`,
		`{"decision":"answer","messages":["x"],"image_url":"file:///etc/passwd"}`,
		`{"decision":"answer","messages":["x"],"image_url":"https://"}`,
		`{"decision":"answer","messages":["x"],"image_url":"https://example.com/` + strings.Repeat("a", maxImageURLLen+1) + `"}`,
	} {
		if _, err := ParseTurnResult([]byte(bad)); err == nil {
			t.Fatalf("недопустимый image_url принят: %s", bad)
		}
	}
}

func TestStripInertImageURL(t *testing.T) {
	for _, decision := range []string{DecisionReact, DecisionIgnore, DecisionDefer} {
		raw := `{"decision":"` + decision + `","image_url":"https://cdn.example.com/a.jpg"`
		switch decision {
		case DecisionReact:
			raw += `,"reaction":"🔥"`
		case DecisionDefer:
			raw += `,"defer_hours":1`
		}
		raw += `}`
		result, err := ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("%s: разбор не удался: %v", decision, err)
		}
		if result.ImageURL != "" {
			t.Fatalf("%s: image_url должен быть обнулён, получено %q", decision, result.ImageURL)
		}
	}
}
