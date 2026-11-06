// Тесты дампа модельных вызовов (40.1): перехват на уровне HTTP-транспорта
// пишет ровно то, что ушло и пришло; ключ API в файл не попадает; заголовок
// x-opencode-session — попадает; имена файлов различают ход и фоновую задачу.
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
	"time"

	"github.com/luckystrker/ago/config"
)

// chatReply — ответ OpenAI-совместимого провайдера с reasoning_content.
const chatReply = `{"id":"resp-1","model":"test-model","choices":[{"finish_reason":"stop","message":{"content":"{\"decision\":\"ignore\"}","reasoning_content":"думаю про кашу"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

// installForTest ставит обёртку на http.DefaultTransport и возвращает её;
// глобальный транспорт восстанавливается после теста.
func installForTest(t *testing.T, d *Dumper) *Transport {
	t.Helper()
	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })
	InstallDefaultTransport(d)
	tr, ok := http.DefaultTransport.(*Transport)
	if !ok {
		t.Fatalf("транспорт не установлен: %T", http.DefaultTransport)
	}
	return tr
}

// chatServer отвечает chatReply, запоминая тела и заголовки запросов.
func chatServer(t *testing.T, bodies *[]string, headers *[]http.Header) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(raw))
		*headers = append(*headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	t.Cleanup(server.Close)
	return server
}

// readRecord читает единственный дамп из каталога: метаданные и сырые тела.
func readRecord(t *testing.T, dir string) (Record, []byte, []byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("каталог дампа не читается: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 {
		t.Fatalf("файлов дампа %v, хочу один", names)
	}
	raw, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("дамп не читается: %v", err)
	}
	var file struct {
		Record
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("дамп не JSON: %v\n%s", err, raw)
	}
	return file.Record, file.Request, file.Response
}

func TestTransportDumpsTurnCall(t *testing.T) {
	dir := t.TempDir()
	dumpDir := filepath.Join(dir, "dumps")
	d, err := NewDumper(dumpDir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	installForTest(t, d)

	var bodies []string
	var headers []http.Header
	server := chatServer(t, &bodies, &headers)

	ctx := WithCall(context.Background(), NewCall("turn", "246"))
	params := config.CompatParams{
		APIKey:  "super-secret-key",
		BaseURL: server.URL,
		Model:   "test-model",
		ExtraHeaders: map[string]string{
			"x-opencode-session": "session-abc",
		},
	}
	resp, err := config.RunChatCompat(ctx, params, nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("вызов модели упал: %v", err)
	}
	if resp.Message == nil || resp.Message.Content != `{"decision":"ignore"}` {
		t.Fatalf("ответ модели не разобран транспортом: %+v", resp.Message)
	}

	rec, dumpedRaw, responseRaw := readRecord(t, dumpDir)
	if rec.TurnID != 246 {
		t.Fatalf("turn_id = %d, хочу 246", rec.TurnID)
	}
	if rec.Call != 1 {
		t.Fatalf("call = %d, хочу 1", rec.Call)
	}
	if rec.Status != http.StatusOK {
		t.Fatalf("status = %d, хочу 200", rec.Status)
	}
	if rec.At.IsZero() {
		t.Fatal("в дампе нет времени вызова")
	}
	if rec.LatencyMs < 0 {
		t.Fatalf("latency_ms = %d", rec.LatencyMs)
	}

	// Запрос — ровно то, что ушло на провод (messages, model, response_format…).
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("тело запроса не JSON: %v", err)
	}
	var dumped map[string]any
	if err := json.Unmarshal(dumpedRaw, &dumped); err != nil {
		t.Fatalf("request в дампе не JSON: %v\n%s", err, dumpedRaw)
	}
	if sent["model"] != dumped["model"] {
		t.Fatalf("request в дампе не совпадает с отправленным: %v vs %v", dumped, sent)
	}
	if _, ok := dumped["messages"]; !ok {
		t.Fatalf("в request дампа нет messages: %s", dumpedRaw)
	}
	// Ответ — сырое тело провайдера.
	if !strings.Contains(string(responseRaw), `"reasoning_content":"думаю про кашу"`) {
		t.Fatalf("в response дампа нет сырого ответа: %s", responseRaw)
	}

	// Заголовки: ключ API не пишется, идентификатор сессии — можно.
	for name := range rec.Headers {
		if strings.EqualFold(name, "Authorization") {
			t.Fatalf("в дамп попал заголовок Authorization: %v", rec.Headers)
		}
	}
	if got := rec.Headers["x-opencode-session"]; got != "session-abc" {
		t.Fatalf("x-opencode-session в дампе = %q (%v)", got, rec.Headers)
	}

	// Файл turn-<id>-call-<n>.json, права 0600; каталог 0700.
	entries, _ := os.ReadDir(dumpDir)
	if len(entries) != 1 || entries[0].Name() != "turn-246-call-1.json" {
		t.Fatalf("имя дампа неверное: %v", entries)
	}
	info, err := os.Stat(filepath.Join(dumpDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("файл дампа не найден: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("права файла дампа %o, хочу 600", perm)
	}
	dirInfo, err := os.Stat(dumpDir)
	if err != nil {
		t.Fatalf("каталог дампа не найден: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("права каталога дампа %o, хочу 700", perm)
	}
}

func TestTransportDumpsBackgroundTaskAndCountsCalls(t *testing.T) {
	dumpDir := t.TempDir()
	d, err := NewDumper(dumpDir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	installForTest(t, d)

	var bodies []string
	var headers []http.Header
	server := chatServer(t, &bodies, &headers)

	call := NewCall("bg", "intention:42")
	ctx := WithCall(context.Background(), call)
	params := config.CompatParams{APIKey: "k", BaseURL: server.URL, Model: "m"}
	for i := 0; i < 2; i++ {
		if _, err := config.RunChatCompat(ctx, params, nil, nil, config.CallOptions{}); err != nil {
			t.Fatalf("вызов %d упал: %v", i+1, err)
		}
	}

	for _, want := range []string{"bg-intention-42-call-1.json", "bg-intention-42-call-2.json"} {
		if _, err := os.Stat(filepath.Join(dumpDir, want)); err != nil {
			t.Fatalf("нет файла %s: %v", want, err)
		}
	}
}

func TestTransportPassesThroughWithoutCall(t *testing.T) {
	dumpDir := t.TempDir()
	d, err := NewDumper(dumpDir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	installForTest(t, d)

	var bodies []string
	var headers []http.Header
	server := chatServer(t, &bodies, &headers)

	// Без Call в контексте вызов не пишется (например, embeddings).
	params := config.CompatParams{APIKey: "k", BaseURL: server.URL, Model: "m"}
	if _, err := config.RunChatCompat(context.Background(), params, nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("вызов упал: %v", err)
	}
	entries, _ := os.ReadDir(dumpDir)
	if len(entries) != 0 {
		t.Fatalf("без Call в контексте появились дампы: %v", entries)
	}
}

func TestDumperPruneRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	old := filepath.Join(dir, "turn-1-call-1.json")
	fresh := filepath.Join(dir, "turn-2-call-1.json")
	if err := os.WriteFile(old, []byte("{}"), 0o600); err != nil {
		t.Fatalf("старый файл не создан: %v", err)
	}
	if err := os.WriteFile(fresh, []byte("{}"), 0o600); err != nil {
		t.Fatalf("свежий файл не создан: %v", err)
	}
	now := time.Now()
	stale := now.Add(-Retention - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("время файла не подменено: %v", err)
	}

	removed, err := d.Prune(now, Retention)
	if err != nil {
		t.Fatalf("ретенция упала: %v", err)
	}
	if removed != 1 {
		t.Fatalf("удалено %d файлов, хочу 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("старый дамп не удалён: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("свежий дамп удалён: %v", err)
	}
}

// TestTransportExtractsReasoningContent — этап 40.3: ago v0.6.0 в не-стрим
// ответе читает только message.reasoning, а провайдер отдаёт
// reasoning_content. Транспорт достаёт его сам — иначе journal хода пуст.
func TestTransportExtractsReasoningContent(t *testing.T) {
	installForTest(t, nil) // дамп выключен, сбор reasoning — всегда

	var bodies []string
	var headers []http.Header
	server := chatServer(t, &bodies, &headers)

	call := NewCall("turn", "1")
	ctx := WithCall(context.Background(), call)
	params := config.CompatParams{APIKey: "k", BaseURL: server.URL, Model: "m"}
	if _, err := config.RunChatCompat(ctx, params, nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("вызов упал: %v", err)
	}
	if got := call.TakeReasoning(); got != "думаю про кашу" {
		t.Fatalf("reasoning из транспорта = %q, хочу reasoning_content ответа", got)
	}
	if got := call.TakeReasoning(); got != "" {
		t.Fatalf("повторный Take вернул %q, хочу пусто", got)
	}
}

// TestTakeReasoningPrefersMiddleware — если ago отдал reasoning событием,
// транспортный дубль не мешает: берётся собранное middleware, накопитель
// очищается, чтобы repair не получил reasoning первой попытки.
func TestTakeReasoningPrefersMiddleware(t *testing.T) {
	call := NewCall("turn", "1")
	call.AddReasoning("из транспорта")
	if got := TakeReasoning(call, "из middleware"); got != "из middleware" {
		t.Fatalf("reasoning = %q, хочу middleware", got)
	}
	if got := call.TakeReasoning(); got != "" {
		t.Fatalf("накопитель не очищен: %q", got)
	}

	call2 := NewCall("turn", "2")
	call2.AddReasoning("из транспорта")
	if got := TakeReasoning(call2, ""); got != "из транспорта" {
		t.Fatalf("фолбэк на транспорт = %q", got)
	}
}
