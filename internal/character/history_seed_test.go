// Юнит-тест провода истории (находка 2026-10-31: BaseContext.History собирался
// и выбрасывался — модель каждый ход видела только system и текущую пачку,
// разговор шёл с амнезией). История чата засеивается в поток запуска и доходит
// до провода настоящими user/assistant ходами ПЕРЕД пачкой: BuildMessages ago
// разворачивает ModelRequest/ModelResponse из истории потока (§11.1 п.5).
package character

import (
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
)

func seedAndBuild(t *testing.T, hist []HistoryMessage, batch string) []map[string]any {
	t.Helper()
	s, err := historyStream(hist)
	if err != nil {
		t.Fatalf("история не посеяна: %v", err)
	}
	if err := s.Send(events.ModelRequest{Parts: []events.Input{events.TextInput{Content: batch}}}); err != nil {
		t.Fatalf("пачка не отправлена: %v", err)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("история потока не читается: %v", err)
	}
	msgs, err := config.BuildMessages([]string{"system"}, evs)
	if err != nil {
		t.Fatalf("провод не собран: %v", err)
	}
	return msgs
}

func wireRole(t *testing.T, m map[string]any) string {
	t.Helper()
	role, _ := m["role"].(string)
	return role
}

func wireText(t *testing.T, m map[string]any) string {
	t.Helper()
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		out := ""
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if pm["type"] == "text" {
					if s, ok := pm["text"].(string); ok {
						out += s
					}
				}
			}
		}
		return out
	default:
		t.Fatalf("неожиданный content: %T", m["content"])
		return ""
	}
}

func TestHistorySeededOnWire(t *testing.T) {
	hist := []HistoryMessage{
		{Role: "user", Content: "смотри какой закат сфоткал"},
		{Role: "assistant", Content: "бля круто"},
		{Role: "user", Content: "это где?"},
	}
	msgs := seedAndBuild(t, hist, "а сегодня что?")
	if len(msgs) != 5 {
		t.Fatalf("на проводе %d сообщений, want 5 (system + 3 истории + пачка)", len(msgs))
	}
	wantRoles := []string{"system", "user", "assistant", "user", "user"}
	for i, want := range wantRoles {
		if got := wireRole(t, msgs[i]); got != want {
			t.Fatalf("роль[%d] = %q, want %q", i, got, want)
		}
	}
	wantTexts := []string{
		"system",
		"смотри какой закат сфоткал",
		"бля круто",
		"это где?",
		"а сегодня что?",
	}
	for i, want := range wantTexts[1:] {
		if got := wireText(t, msgs[i+1]); got != want {
			t.Fatalf("текст[%d] = %q, want %q", i+1, got, want)
		}
	}
}

func TestHistorySeedEmptyYieldsSystemAndBatchOnly(t *testing.T) {
	msgs := seedAndBuild(t, nil, "привет")
	if len(msgs) != 2 {
		t.Fatalf("без истории на проводе %d сообщений, want 2", len(msgs))
	}
}

func TestHistorySeedSkipsUnknownRoles(t *testing.T) {
	hist := []HistoryMessage{
		{Role: "tool", Content: "служебное"},
		{Role: "user", Content: "привет"},
	}
	msgs := seedAndBuild(t, hist, "как дела?")
	if len(msgs) != 3 {
		t.Fatalf("неизвестная роль просочилась на провод: %d сообщений, want 3", len(msgs))
	}
}
