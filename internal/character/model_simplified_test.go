// Юнит-тесты упрощённого повтора 400 «feature is not yet supported» (этап
// 38.6): живой прогон 2026-11-06 — консолидация эпизода чата 548919987 пять
// раз получила 400 invalid_request_error «this feature is not yet supported by
// atlas» и терминально упала. 400 не транзиентен: тот же запрос повторять
// бессмысленно, поэтому один раз он уходит в упрощённом виде (без строгой
// JSON-схемы и инструментов), и с причиной — в лог.
package character

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/response"
)

const unsupported400Body = `{"error":{"type":"invalid_request_error","message":"Upstream request failed: [invalid_request_error] this feature is not yet supported by atlas"}}`

// scriptedServer — провайдер, отдающий запрограммированные ответы по порядку;
// тело каждого запроса запоминается.
func scriptedServer(t *testing.T, replies []func(w http.ResponseWriter)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var n atomic.Int32
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		idx := int(n.Add(1)) - 1
		if idx < len(replies) {
			replies[idx](w)
			return
		}
		okReply(w)
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

func okReply(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"id":"x","model":"test","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
}

func reject400(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(unsupported400Body))
}

// structuredOpts — запрос со строгой схемой и инструментом (консолидация).
func structuredOpts(t *testing.T) config.CallOptions {
	t.Helper()
	tool := testTool(t, "memory_search")
	schemas, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("схема инструмента не получена: %v", err)
	}
	return config.CallOptions{
		Tools: schemas,
		ResponseSchema: response.RawSchema(map[string]any{
			"type":       "object",
			"properties": map[string]any{"summary": map[string]any{"type": "string"}},
			"required":   []string{"summary"},
		}, response.WithRawName("ConsolidationProposal")),
	}
}

func TestChatClientSimplifiedRetryOnUnsupported400(t *testing.T) {
	server, bodies := scriptedServer(t, []func(http.ResponseWriter){reject400})
	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.Logf = func(string, ...any) {}
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}

	if _, err := client.Call(t.Context(), nil, nil, structuredOpts(t)); err != nil {
		t.Fatalf("упрощённый повтор не спас вызов: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("провайдер получил %d запросов, хочу 2 (один упрощённый повтор)", len(*bodies))
	}
	first, second := (*bodies)[0], (*bodies)[1]
	if _, has := first["response_format"]; !has {
		t.Fatalf("первый запрос ушёл без строгой схемы: %v", first)
	}
	if _, has := first["tools"]; !has {
		t.Fatalf("первый запрос ушёл без инструментов: %v", first)
	}
	if _, has := second["response_format"]; has {
		t.Fatalf("упрощённый повтор обязан идти без response_format: %v", second)
	}
	if _, has := second["tools"]; has {
		t.Fatalf("упрощённый повтор обязан идти без tools: %v", second)
	}
}

func TestChatClientSimplifiedRetryOnlyOnce(t *testing.T) {
	server, bodies := scriptedServer(t, []func(http.ResponseWriter){reject400, reject400})
	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.Logf = func(string, ...any) {}
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	_, err = client.Call(t.Context(), nil, nil, structuredOpts(t))
	if err == nil || !strings.Contains(err.Error(), "not yet supported") {
		t.Fatalf("упрощённый повтор не вернул исходную 400: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("провайдер получил %d запросов, хочу 2 (один упрощённый повтор, не серию)", len(*bodies))
	}
}

func TestChatClientNoSimplifiedRetryOnOther400(t *testing.T) {
	server, bodies := scriptedServer(t, []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid value: temperature"}}`))
		},
	})
	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.Logf = func(string, ...any) {}
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, structuredOpts(t)); err == nil {
		t.Fatal("прочие 400 обязаны возвращаться без повтора")
	}
	if len(*bodies) != 1 {
		t.Fatalf("провайдер получил %d запросов, хочу 1", len(*bodies))
	}
}

// TestIsPermanentProviderError — классификация 400 для фона: 400/401/403 —
// постоянные (повтор с тем же контекстом бессмыслен), 429 и 5xx — нет.
func TestIsPermanentProviderError(t *testing.T) {
	cases := map[string]bool{
		"config: chat completions returned 400: " + unsupported400Body: true,
		"config: chat completions returned 401: unauthorized":          true,
		"config: chat completions returned 403: forbidden":             true,
		"config: chat completions returned 429: slow down":             false,
		"config: chat completions returned 500: oops":                  false,
		"config: chat completions returned 502: oops":                  false,
		"net: dial tcp: connection refused":                            false,
		"консолидация: пустое summary":                                 false,
	}
	for msg, want := range cases {
		if got := IsPermanentProviderError(errors.New(msg)); got != want {
			t.Fatalf("IsPermanentProviderError(%q) = %v, хочу %v", msg, got, want)
		}
	}
	if IsPermanentProviderError(nil) {
		t.Fatal("nil — не постоянная ошибка")
	}
}
