// Юнит-тесты короткого повтора 429 внутри хода (этап 38.5): провайдер шлёт
// «retry shortly (after 5 s)», транспорт ago с MODEL_MAX_RETRIES=0 не повторяет
// — ход возвращался в очередь с паузой 30 с и отвечал через минуты. Повтор
// живёт в обёртке модельного клиента (общая для хода и фона), поэтому резерв
// бюджета middleware не расходуется второй раз: middleware видит один вызов.
package character

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
)

// short429Body — тело отказа провайдера с подсказкой паузы (боевой формат).
const short429Body = `{"error":{"type":"server_error","message":"Upstream request failed: [prefill_cap_refused] the cell is at capacity; retry shortly (after 5 s)"}}`

func TestModelRetryAfterParsing(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want time.Duration
		ok   bool
	}{
		{"боевой формат 429", "config: chat completions returned 429: " + short429Body, 5 * time.Second, true},
		{"retry after N s", "config: chat completions returned 429: too many requests, retry after 3 s", 3 * time.Second, true},
		{"Retry-After: N", "config: chat completions returned 429: Retry-After: 7", 7 * time.Second, true},
		{"дробные секунды", "config: chat completions returned 429: after 1.5 s", 1500 * time.Millisecond, true},
		{"пауза больше потолка", "config: chat completions returned 429: after 30 s", 0, false},
		{"паузы нет", "config: chat completions returned 429: slow down", 0, false},
		{"не 429", "config: chat completions returned 500: after 5 s", 0, false},
		{"другая ошибка", "httpx: request failed: dial tcp: connection refused", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := modelRetryAfter(errors.New(tc.err))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("modelRetryAfter = %v/%v, хочу %v/%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// retryServer — провайдер с программируемыми ответами: 429 на первые fails
// запросов, затем валидный ответ. Считает запросы и запоминает тело.
func retryServer(t *testing.T, fails int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= fails {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","model":"test","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// stubRetrySleep подменяет паузу повтора: тесты не должны спать секундами.
func stubRetrySleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	orig := modelRetrySleep
	modelRetrySleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	t.Cleanup(func() { modelRetrySleep = orig })
	return &slept
}

func TestChatClientRetriesShort429(t *testing.T) {
	slept := stubRetrySleep(t)
	server, calls := retryServer(t, 1, short429Body)

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("короткий 429 не повторён: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("провайдер получил %d запросов, хочу 2 (один повтор внутри хода)", got)
	}
	if len(*slept) != 1 || (*slept)[0] != 5*time.Second {
		t.Fatalf("паузы повтора = %v, хочу [5s] (N из подсказки провайдера)", *slept)
	}
}

func TestChatClientRetriesShort429TwiceThenFails(t *testing.T) {
	slept := stubRetrySleep(t)
	server, calls := retryServer(t, 100, short429Body)

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, config.CallOptions{}); err == nil {
		t.Fatal("упорный 429 обязан вернуть ошибку в очередь")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("провайдер получил %d запросов, хочу 3 (две короткие попытки, не больше)", got)
	}
	if len(*slept) != modelRetryAttempts {
		t.Fatalf("пауз повтора %d, хочу %d", len(*slept), modelRetryAttempts)
	}
}

func TestChatClientNoRetryForLong429(t *testing.T) {
	slept := stubRetrySleep(t)
	server, calls := retryServer(t, 100, `{"error":{"message":"retry shortly (after 30 s)"}}`)

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, config.CallOptions{}); err == nil {
		t.Fatal("429 без короткой паузы обязан вернуть ошибку сразу")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("провайдер получил %d запросов, хочу 1 (пауза 30 с — не короткая)", got)
	}
	if len(*slept) != 0 {
		t.Fatalf("были паузы %v, хочу ни одной", *slept)
	}
}

func TestChatClient429RetryStopsOnCancel(t *testing.T) {
	var slept []time.Duration
	orig := modelRetrySleep
	modelRetrySleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return context.Canceled // ход отменён во время паузы
	}
	t.Cleanup(func() { modelRetrySleep = orig })
	server, calls := retryServer(t, 100, short429Body)

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	_, err = client.Call(t.Context(), nil, nil, config.CallOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена во время паузы не проброшена: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("после отмены провайдер получил %d запросов, хочу 1", got)
	}
	if len(slept) != 1 {
		t.Fatalf("пауз %d, хочу 1", len(slept))
	}
}
