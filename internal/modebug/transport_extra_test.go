// Дополнительные тесты транспорта (40.1/40.3): безопасность дампа (ключ не
// пишется, приватный текст — по замыслу), идемпотентность установки, сбой
// записи, транспортные ошибки и стрим-SSE.
package modebug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sseServer отдаёт стрим Chat Completions: reasoning, две части content и
// [DONE]; заголовок — text/event-stream.
func sseServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"думаю\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"при\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"вет\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

// postThrough гонит один запрос через транспорт с заданным ctx и телом.
func postThrough(t *testing.T, tr *Transport, ctx context.Context, target, body string,
	headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("запрос не собран: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("запрос через транспорт упал: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("тело ответа не прочитано: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("тело ответа не закрыто: %v", err)
	}
	return resp, data
}

// TestTransportDumpHidesKeyKeepsPrivateText — ключ API не попадает в файл ни
// заголовком, ни подстрокой; приватный текст собеседника — попадает (в этом
// и смысл дампа).
func TestTransportDumpHidesKeyKeepsPrivateText(t *testing.T) {
	dir := t.TempDir()
	dumpDir := filepath.Join(dir, "dumps")
	d, err := NewDumper(dumpDir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	tr := &Transport{Base: http.DefaultTransport, Dumper: d}

	const (
		secret  = "super-secret-key"
		private = "секрет: мне очень грустно сегодня"
	)
	var bodies []string
	var headers []http.Header
	server := chatServer(t, &bodies, &headers)

	ctx := WithCall(context.Background(), NewCall("turn", "246"))
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%q}]}`, private)
	postThrough(t, tr, ctx, server.URL, body, map[string]string{
		"Authorization":      "Bearer " + secret,
		"x-opencode-session": "sess-private",
	})

	entries, err := os.ReadDir(dumpDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("файлов дампа %v (%v), хочу один", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(dumpDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("дамп не прочитан: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("ключ API попал в файл дампа: %s", raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "authorization") {
		t.Fatalf("заголовок Authorization попал в дамп: %s", raw)
	}
	if !strings.Contains(string(raw), private) {
		t.Fatalf("приватный текст собеседника не попал в дамп (нужен для отладки): %s", raw)
	}
	if !strings.Contains(string(raw), "sess-private") {
		t.Fatalf("x-opencode-session не попал в дамп (нужен replay): %s", raw)
	}
}

// TestInstallDefaultTransportIdempotent — повторная установка не оборачивает
// цепочку второй раз: обёртка одна, Base — исходный транспорт, дампер
// заменяется.
func TestInstallDefaultTransportIdempotent(t *testing.T) {
	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })
	http.DefaultTransport = saved // страховка от грязи предыдущих тестов

	d1, err := NewDumper(filepath.Join(t.TempDir(), "one"), nil)
	if err != nil {
		t.Fatalf("первый дампер: %v", err)
	}
	d2, err := NewDumper(filepath.Join(t.TempDir(), "two"), nil)
	if err != nil {
		t.Fatalf("второй дампер: %v", err)
	}

	tr1 := InstallDefaultTransport(d1)
	if got := http.DefaultTransport.(*Transport); got != tr1 {
		t.Fatal("первый вызов не поставил обёртку")
	}
	if tr1.Base != saved {
		t.Fatalf("Base = %T, хочу исходный %T", tr1.Base, saved)
	}

	tr2 := InstallDefaultTransport(d2)
	if tr2 != tr1 {
		t.Fatal("второй вызов создал новую обёртку (цепочка удвоилась)")
	}
	if tr1.Base != saved {
		t.Fatalf("второй вызов завернул Base: %T", tr1.Base)
	}
	if tr1.Dumper != d2 {
		t.Fatal("второй вызов не заменил дампер")
	}

	tr3 := InstallDefaultTransport(nil)
	if tr3 != tr1 || tr1.Dumper != nil {
		t.Fatal("установка nil-дампера должна только выключить дамп")
	}
}

// TestTransportErrorIsDumped — транспортная ошибка тоже видна в дампе (status
// 0, error), иначе «провайдер недоступен» не отличить от пустого ответа.
func TestTransportErrorIsDumped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	tr := &Transport{Base: failingRT{err: errors.New("сеть легла")}, Dumper: d}
	ctx := WithCall(context.Background(), NewCall("turn", "9"))
	if _, err := tr.RoundTrip(mustRequest(t, ctx)); err == nil {
		t.Fatal("ошибка транспорта проглочена")
	}
	rec, _, _ := readRecord(t, dir)
	if rec.Status != 0 {
		t.Fatalf("status = %d, хочу 0", rec.Status)
	}
	if !strings.Contains(rec.Error, "сеть легла") {
		t.Fatalf("ошибка не записана в дамп: %q", rec.Error)
	}
}

// TestTransportStreamDump — стрим-ответ сворачивается в
// {stream,content,reasoning,chunks}: дамп читаем, тело вызывающему не
// испорчено.
func TestTransportStreamDump(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	tr := &Transport{Base: http.DefaultTransport, Dumper: d}
	server := sseServer(t)

	call := NewCall("turn", "77")
	ctx := WithCall(context.Background(), call)
	resp, body := postThrough(t, tr, ctx, server.URL, `{"model":"m","stream":true}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Вызывающий получил поток как есть.
	if !strings.Contains(string(body), `"content":"при"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("тело стрима испорчено транспортом: %s", body)
	}

	rec, _, responseRaw := readRecord(t, dir)
	if rec.Status != http.StatusOK || rec.Call != 1 {
		t.Fatalf("метаданные дампа стрима: %+v", rec)
	}
	var payload struct {
		Stream    bool     `json:"stream"`
		Content   string   `json:"content"`
		Reasoning string   `json:"reasoning"`
		Chunks    []string `json:"chunks"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(responseRaw, &payload); err != nil {
		t.Fatalf("response дампа стрима не JSON: %v\n%s", err, responseRaw)
	}
	if !payload.Stream {
		t.Fatal("в дампе стрима нет stream=true")
	}
	if payload.Content != "привет" {
		t.Fatalf("content стрима = %q, хочу привет", payload.Content)
	}
	if payload.Reasoning != "думаю" {
		t.Fatalf("reasoning стрима = %q, хочу думаю", payload.Reasoning)
	}
	if len(payload.Chunks) != 3 {
		t.Fatalf("чанков %d, хочу 3 (без [DONE])", len(payload.Chunks))
	}
	if payload.Truncated {
		t.Fatal("короткий стрим помечен truncated")
	}
	// Транспорт собрал reasoning в накопитель вызова (40.3).
	if got := call.TakeReasoning(); got != "думаю" {
		t.Fatalf("reasoning из стрима в Call = %q, хочу думаю", got)
	}
}

// TestTransportStreamReasoningWithoutDump — обёртка ставится всегда ради
// reasoning_content (40.3): при выключенном дампе стрим тоже обязан отдать
// reasoning в накопитель вызова.
func TestTransportStreamReasoningWithoutDump(t *testing.T) {
	tr := &Transport{Base: http.DefaultTransport} // дамп выключен
	server := sseServer(t)

	call := NewCall("turn", "78")
	ctx := WithCall(context.Background(), call)
	postThrough(t, tr, ctx, server.URL, `{"model":"m","stream":true}`, nil)

	if got := call.TakeReasoning(); got != "думаю" {
		t.Fatalf("reasoning из стрима без дампа = %q, хочу думаю: 40.3 не зависит от дампа", got)
	}
}

// TestTransportStreamWithoutCallPassesThrough — стрим без Call в контексте не
// оборачивается и не пишет файл: эмбеддинги/веб/Telegram идут насквозь.
func TestTransportStreamWithoutCallPassesThrough(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	tr := &Transport{Base: http.DefaultTransport, Dumper: d}
	server := sseServer(t)
	_, body := postThrough(t, tr, context.Background(), server.URL, `{"model":"m","stream":true}`, nil)
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("тело стрима без Call испорчено: %s", body)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("без Call появился дамп: %v", entries)
	}
}

// failingRT — транспорт, который всегда падает (сеть/таймаут).
type failingRT struct{ err error }

func (f failingRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func mustRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("запрос не собран: %v", err)
	}
	return req
}
