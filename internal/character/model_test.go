// Юнит-тесты модельного слоя хода (model.go): forced tool-calling на прямом
// обращении (§10.4) — первый вызов модели получает wire-поле tool_choice с
// именем forced-инструмента, остальные — нет (после первого вызова модель
// должна иметь возможность ответить финальным TurnResult). Проведение
// проверяется на реальном движке config.RunChatCompat против httptest-сервера.
package character

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luckystrker/ago/config"
	agotools "github.com/luckystrker/ago/tools"
)

// captureServer собирает тела POST-запросов chat/completions и отвечает
// минимальным валидным ответом движка.
func captureServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("тело запроса не читается: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("тело не JSON: %v", err)
		}
		*bodies = append(*bodies, body)
		_, _ = w.Write([]byte(`{"id":"x","model":"test","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// testTool — схема функции для провода (имя важно, реализация не вызывается).
func testTool(t *testing.T, name string) agotools.Tool {
	t.Helper()
	tool, err := agotools.NewFunctionTool(name, "тестовый инструмент",
		func(args struct {
			Query string `json:"query"`
		}) (string, error) {
			return "[]", nil
		})
	if err != nil {
		t.Fatalf("инструмент не создан: %v", err)
	}
	return tool
}

func TestChatClientForcedToolChoiceFirstCallOnly(t *testing.T) {
	var bodies []map[string]any
	server := captureServer(t, &bodies)

	tool := testTool(t, "memory_search")
	schemas, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("схема инструмента не получена: %v", err)
	}
	opts := config.CallOptions{Tools: schemas}

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.ForcedTool = "memory_search"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}

	ctx := t.Context()
	if _, err := client.Call(ctx, nil, nil, opts); err != nil {
		t.Fatalf("первый вызов упал: %v", err)
	}
	if _, err := client.Call(ctx, nil, nil, opts); err != nil {
		t.Fatalf("второй вызов упал: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("сервер получил %d запросов, хочу 2", len(bodies))
	}

	// Первый вызов: tool_choice = memory_search (механика провайдера, §10.4).
	first, ok := bodies[0]["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("на первом вызове нет tool_choice: %v", bodies[0])
	}
	fn, ok := first["function"].(map[string]any)
	if !ok || first["type"] != "function" || fn["name"] != "memory_search" {
		t.Fatalf("tool_choice неверен: %v", first)
	}
	// Второй вызов: без tool_choice — модель может ответить финальным TurnResult.
	if _, has := bodies[1]["tool_choice"]; has {
		t.Fatalf("второй вызов тоже принуждён: %v", bodies[1]["tool_choice"])
	}
}

// TestChatConfigForceOnlyOnceAcrossRuns — repair-сценарий §11.3: ago создаёт
// клиента заново на каждый запуск (reply.Ask → новый open() → cfg.Create(),
// third_party/ago/agent/run.go), поэтому счётчик вызовов обязан жить на
// конфиге хода: первый вызов ВТОРОГО клиента того же конфига не получает
// tool_choice, иначе repair прямого хода заново форсирует инструмент вместо
// финального TurnResult.
func TestChatConfigForceOnlyOnceAcrossRuns(t *testing.T) {
	var bodies []map[string]any
	server := captureServer(t, &bodies)

	tool := testTool(t, "memory_search")
	schemas, serr := tool.Schemas(nil)
	if serr != nil {
		t.Fatalf("схема инструмента не получена: %v", serr)
	}
	opts := config.CallOptions{Tools: schemas}

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.ForcedTool = "memory_search"

	ctx := t.Context()
	first, err := cfg.Create() // запуск 1: обычный Ask
	if err != nil {
		t.Fatalf("клиент запуска 1 не создан: %v", err)
	}
	if _, err := first.Call(ctx, nil, nil, opts); err != nil {
		t.Fatalf("вызов запуска 1 упал: %v", err)
	}
	second, err := cfg.Create() // запуск 2: repair через reply.Ask — новый клиент
	if err != nil {
		t.Fatalf("клиент запуска 2 (repair) не создан: %v", err)
	}
	if _, err := second.Call(ctx, nil, nil, opts); err != nil {
		t.Fatalf("repair-вызов упал: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("сервер получил %d запросов, хочу 2", len(bodies))
	}
	if _, has := bodies[0]["tool_choice"]; !has {
		t.Fatalf("первый вызов хода потерял tool_choice: %v", bodies[0])
	}
	if _, has := bodies[1]["tool_choice"]; has {
		t.Fatalf("repair-вызов заново форсирует инструмент: %v", bodies[1]["tool_choice"])
	}
}

// TestChatClientDisabledToolsSendToolChoiceNone — после исчерпания лимита
// инструментов вызов уходит с tool_choice="none", но определения инструментов
// и strict-схема остаются (39.2): префикс запроса стабилен, модель отвечает
// финальным TurnResult.
func TestChatClientDisabledToolsSendToolChoiceNone(t *testing.T) {
	var bodies []map[string]any
	server := captureServer(t, &bodies)

	tool := testTool(t, "memory_search")
	schemas, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("схема инструмента не получена: %v", err)
	}
	opts := config.CallOptions{Tools: schemas}

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.ForcedTool = "memory_search"
	cfg.DisableTools()
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, opts); err != nil {
		t.Fatalf("вызов упал: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("сервер получил %d запросов, хочу 1", len(bodies))
	}
	if _, has := bodies[0]["tools"]; !has {
		t.Fatalf("при закрытых инструментах определения не ушли на провод (39.2): %v", bodies[0])
	}
	if choice, ok := bodies[0]["tool_choice"].(string); !ok || choice != "none" {
		t.Fatalf("tool_choice = %v, хочу none", bodies[0]["tool_choice"])
	}
}

func TestChatClientNoForceWithoutTool(t *testing.T) {
	var bodies []map[string]any
	server := captureServer(t, &bodies)

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("вызов упал: %v", err)
	}
	if _, has := bodies[0]["tool_choice"]; has {
		t.Fatalf("обычный ход получил tool_choice: %v", bodies[0]["tool_choice"])
	}
}

func TestChatConfigClone(t *testing.T) {
	cfg := NewChatConfig("https://example.com", "k", "m", 10, 0, 0)
	cfg.ForcedTool = "a"
	cloned, err := cfg.Clone(map[string]any{"forced_tool": "b"})
	if err != nil {
		t.Fatalf("клон не создан: %v", err)
	}
	if cloned.Model() != "m" || cloned.Provider() != config.ProviderOpenAI {
		t.Fatalf("клон потерял параметры: %v %v", cloned.Model(), cloned.Provider())
	}
	cc := cloned.(*ChatConfig)
	if cc.ForcedTool != "b" {
		t.Fatalf("forced_tool клона = %q, хочу b", cc.ForcedTool)
	}
	if _, err := cfg.Clone(map[string]any{"unknown": 1}); err == nil {
		t.Fatalf("неизвестный ключ клона не пойман")
	}
}
