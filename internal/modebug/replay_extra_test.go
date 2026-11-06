// Дополнительные тесты replay (40.2): не-2xx ответ, заголовки из файла не
// подменяют ключ, точный URL из дампа, ошибки загрузки и разбора ответа.
package modebug

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplayerNon2xxReturnsReplyAndError — провайдер ответил 5xx: replay
// обязан вернуть и тело (для диагностики), и ошибку с кодом.
func TestReplayerNon2xxReturnsReplyAndError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"провайдер перегружен"}}`)
	}))
	t.Cleanup(server.Close)

	rep := &Replayer{Endpoint: server.URL, APIKey: "k", Client: server.Client()}
	reply, err := rep.Send(context.Background(), "", map[string]any{"model": "m"}, nil)
	if err == nil {
		t.Fatal("не-2xx ответ не стал ошибкой")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("ошибка не называет код: %v", err)
	}
	if reply == nil {
		t.Fatal("reply не возвращён при не-2xx")
	}
	if reply.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, хочу 503", reply.Status)
	}
	if !strings.Contains(string(reply.Body), "провайдер перегружен") {
		t.Fatalf("тело не-2xx потеряно: %s", reply.Body)
	}
}

// TestReplayerDumpHeadersDoNotOverrideKey — Authorization и Content-Type из
// файла игнорируются: ключ берётся из окружения, тело — JSON. Остальные
// заголовки (x-opencode-session) проходят.
func TestReplayerDumpHeadersDoNotOverrideKey(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(server.Close)

	rep := &Replayer{Endpoint: server.URL, APIKey: "real-key", Client: server.Client()}
	headers := map[string]string{
		"authorization":      "Bearer fake-key-from-file",
		"content-type":       "text/plain",
		"accept":             "*/*",
		"x-opencode-session": "sess-1",
	}
	if _, err := rep.Send(context.Background(), "", map[string]any{"model": "m"}, headers); err != nil {
		t.Fatalf("replay упал: %v", err)
	}
	if auth := got.Get("Authorization"); auth != "Bearer real-key" {
		t.Fatalf("Authorization = %q, хочу ключ из окружения", auth)
	}
	if ct := got.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, хочу application/json", ct)
	}
	if session := got.Get("x-opencode-session"); session != "sess-1" {
		t.Fatalf("x-opencode-session = %q", session)
	}
}

// TestReplayerEmptyAPIKeyDoesNotSendAuthorization — пустой ключ не превращается
// в «Bearer ».
func TestReplayerEmptyAPIKeyDoesNotSendAuthorization(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(server.Close)

	rep := &Replayer{Endpoint: server.URL, Client: server.Client()}
	if _, err := rep.Send(context.Background(), "", map[string]any{"model": "m"}, nil); err != nil {
		t.Fatalf("replay упал: %v", err)
	}
	if auth := got.Get("Authorization"); auth != "" {
		t.Fatalf("пустой ключ дал Authorization %q", auth)
	}
}

// TestReplayerNoTarget — ни url в дампе, ни MODEL_ENDPOINT: явная ошибка, а не
// запрос в пустоту.
func TestReplayerNoTarget(t *testing.T) {
	rep := &Replayer{APIKey: "k"}
	if _, err := rep.Send(context.Background(), "", map[string]any{"model": "m"}, nil); err == nil {
		t.Fatal("пустой endpoint не стал ошибкой")
	}
}

// TestReplayerUsesDumpURLVerbatim — url из дампа уже с /chat/completions:
// второй раз путь не добавляется.
func TestReplayerUsesDumpURLVerbatim(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(server.Close)

	rep := &Replayer{Endpoint: server.URL + "/other", APIKey: "k", Client: server.Client()}
	target := server.URL + "/v1/chat/completions"
	if _, err := rep.Send(context.Background(), target, map[string]any{"model": "m"}, nil); err != nil {
		t.Fatalf("replay упал: %v", err)
	}
	if path != "/v1/chat/completions" {
		t.Fatalf("путь = %q, хочу /v1/chat/completions (url дампа)", path)
	}
}

// TestReplayerMarshalError — тело с несериализуемым значением: явная ошибка
// раньше сети.
func TestReplayerMarshalError(t *testing.T) {
	rep := &Replayer{Endpoint: "http://127.0.0.1:1", APIKey: "k"}
	_, err := rep.Send(context.Background(), "", map[string]any{"bad": make(chan int)}, nil)
	if err == nil || !strings.Contains(err.Error(), "не сериализуется") {
		t.Fatalf("ошибка сериализации не названа: %v", err)
	}
}

// TestLoadDumpErrors — битый/неполный/отсутствующий дамп: ошибка с путём.
func TestLoadDumpErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadDump(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("отсутствующий дамп не стал ошибкой")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("не json"), 0o600); err != nil {
		t.Fatalf("битый дамп не создан: %v", err)
	}
	if _, err := LoadDump(bad); err == nil || !strings.Contains(err.Error(), "не JSON") {
		t.Fatalf("битый дамп: %v", err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"turn_id":1}`), 0o600); err != nil {
		t.Fatalf("пустой дамп не создан: %v", err)
	}
	if _, err := LoadDump(empty); err == nil || !strings.Contains(err.Error(), "пустой request") {
		t.Fatalf("пустой request: %v", err)
	}
}

// TestLoadDumpRejectsNullRequest — request:null — не запрос: без проверки он
// пройдёт как «пустой» map и уронит абляцию temperature присваиванием в nil.
func TestLoadDumpRejectsNullRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(path, []byte(`{"turn_id":1,"request":null}`), 0o600); err != nil {
		t.Fatalf("дамп не создан: %v", err)
	}
	if _, err := LoadDump(path); err == nil {
		t.Fatal("request:null принят как дамп — ожидаю ошибку")
	}
}

// TestParseChatResponse — content/reasoning, фолбэк на message.reasoning и
// молчание на мусоре.
func TestParseChatResponse(t *testing.T) {
	got := ParseChatResponse([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok","reasoning_content":"думаю"}}],"usage":{"total_tokens":3}}`))
	if got.Content != "ok" || got.Reasoning != "думаю" || got.FinishReason != "stop" {
		t.Fatalf("ответ разобран неверно: %+v", got)
	}
	if got.Usage["total_tokens"] != float64(3) {
		t.Fatalf("usage = %v", got.Usage)
	}

	fallback := ParseChatResponse([]byte(`{"choices":[{"message":{"content":"ok","reasoning":"диалект"}}]}`))
	if fallback.Reasoning != "диалект" {
		t.Fatalf("фолбэк message.reasoning = %q", fallback.Reasoning)
	}

	for _, raw := range []string{"не json", `{}`, `{"choices":[]}`, `{"choices":[{"message":{}}]}`} {
		got := ParseChatResponse([]byte(raw))
		if got.Content != "" || got.Reasoning != "" {
			t.Fatalf("на %q разобран непустой ответ: %+v", raw, got)
		}
	}
}

// TestEnsureChatPath — база получает /chat/completions, готовый путь не
// дублируется, пустая база остаётся пустой.
func TestEnsureChatPath(t *testing.T) {
	cases := map[string]string{
		"https://api.example/v1":                   "https://api.example/v1/chat/completions",
		"https://api.example/v1/":                  "https://api.example/v1/chat/completions",
		"https://api.example/v1/chat/completions":  "https://api.example/v1/chat/completions",
		"https://api.example/v1/chat/completions/": "https://api.example/v1/chat/completions",
		"": "",
	}
	for in, want := range cases {
		if got := ensureChatPath(in); got != want {
			t.Fatalf("ensureChatPath(%q) = %q, хочу %q", in, got, want)
		}
	}
}
