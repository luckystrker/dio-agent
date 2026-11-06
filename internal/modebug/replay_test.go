// Тесты replay (40.2): дамп читается, запрос уходит тому же провайдеру с тем
// же ключом и x-opencode-session, ответ разбирается на content/reasoning.
package modebug

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "turn-246-call-1.json")
	raw := `{"turn_id":246,"call":1,"at":"2026-11-06T12:00:00Z","url":"https://example.test/v1/chat/completions",
		"headers":{"x-opencode-session":"sess-1"},
		"request":{"model":"m","messages":[{"role":"user","content":"привет"}]},
		"response":{"choices":[]},"status":200,"latency_ms":1200}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("дамп не создан: %v", err)
	}
	d, err := LoadDump(path)
	if err != nil {
		t.Fatalf("дамп не прочитан: %v", err)
	}
	if d.TurnID != 246 || d.Call != 1 || d.URL == "" {
		t.Fatalf("метаданные дампа: %+v", d)
	}
	if d.Headers["x-opencode-session"] != "sess-1" {
		t.Fatalf("заголовки дампа: %v", d.Headers)
	}
	var body map[string]any
	if err := json.Unmarshal(d.Request, &body); err != nil {
		t.Fatalf("request дампа не JSON: %v", err)
	}
	if body["model"] != "m" {
		t.Fatalf("request дампа: %v", body)
	}
}

func TestReplayerSendsRequestWithSessionAndAblations(t *testing.T) {
	var got map[string]any
	var auth, session string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		auth = r.Header.Get("Authorization")
		session = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"decision\":\"ignore\"}","reasoning_content":"потому что"}}]}`))
	}))
	t.Cleanup(server.Close)

	req := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "[пт 06.11 12:01] #m1 Коля: привет"},
		},
	}
	rep := &Replayer{Endpoint: server.URL + "/v1", APIKey: "k", Client: server.Client()}
	reply, err := rep.Send(context.Background(), "", req, map[string]string{"x-opencode-session": "sess-1"})
	if err != nil {
		t.Fatalf("replay упал: %v", err)
	}
	if !strings.HasSuffix(reply.URL, "/v1/chat/completions") {
		t.Fatalf("URL replay = %q", reply.URL)
	}
	if auth != "Bearer k" {
		t.Fatalf("Authorization = %q", auth)
	}
	if session != "sess-1" {
		t.Fatalf("x-opencode-session = %q", session)
	}
	if got["model"] != "m" {
		t.Fatalf("запрос не дошёл: %v", got)
	}
	parsed := ParseChatResponse(reply.Body)
	if parsed.Content != `{"decision":"ignore"}` || parsed.Reasoning != "потому что" {
		t.Fatalf("ответ разобран неверно: %+v", parsed)
	}
}

func TestReplayerAppendsChatPath(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(server.Close)

	rep := &Replayer{Endpoint: server.URL + "/api/v1/", APIKey: "k", Client: server.Client()}
	if _, err := rep.Send(context.Background(), "", map[string]any{"model": "m"}, nil); err != nil {
		t.Fatalf("replay упал: %v", err)
	}
	if path != "/api/v1/chat/completions" {
		t.Fatalf("путь по умолчанию = %q", path)
	}
}

// TestReplayerRejectsForeignHost — url из файла дампа не должен уводить ключ
// из .env на чужой хост (файл можно скачать/подменить).
func TestReplayerRejectsForeignHost(t *testing.T) {
	rep := &Replayer{Endpoint: "https://provider.example/v1", APIKey: "secret-key"}
	_, err := rep.Send(context.Background(), "https://evil.example/v1/chat/completions",
		map[string]any{"model": "m"}, nil)
	if err == nil {
		t.Fatal("чужой хост принят — ключ ушёл бы наружу")
	}
	if !strings.Contains(err.Error(), "чужому хосту") {
		t.Fatalf("ошибка не объясняет причину: %v", err)
	}
}

// TestReplayerForcesNonStream — replay всегда не-стрим: иначе SSE-ответ
// разберётся как пустой content и вердикт будет ложным.
func TestReplayerForcesNonStream(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(server.Close)

	rep := &Replayer{Endpoint: server.URL, APIKey: "k", Client: server.Client()}
	body := map[string]any{"model": "m", "stream": true, "stream_options": map[string]any{"include_usage": true}}
	if _, err := rep.Send(context.Background(), "", body, nil); err != nil {
		t.Fatalf("replay упал: %v", err)
	}
	if got["stream"] != false {
		t.Fatalf("stream не выключен: %v", got["stream"])
	}
	if _, has := got["stream_options"]; has {
		t.Fatalf("stream_options остался: %v", got["stream_options"])
	}
}
