// Юнит-тесты тонкого клиента Bot API через httptest: путь и payload метода,
// парсинг ответа, 429 с retry_after, подтверждённые ошибки, таймаут вызова.
// Поведение — src/telegram/api.test.ts.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer поднимает Bot API-заглушку и клиент над ней; ответ задаёт
// обработчик.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient("TESTTOKEN", server.URL, server.Client()), server
}

func TestClientSendMessage(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777,"chat":{"id":42}}}`))
	})

	messageID, err := client.SendMessage(context.Background(), 42, "привет")
	if err != nil {
		t.Fatalf("sendMessage не удался: %v", err)
	}
	if messageID != 777 {
		t.Fatalf("message_id = %d, хочу 777", messageID)
	}
	if gotPath != "/botTESTTOKEN/sendMessage" {
		t.Fatalf("путь = %s", gotPath)
	}
	if gotBody["chat_id"].(float64) != 42 || gotBody["text"] != "привет" {
		t.Fatalf("payload: %v", gotBody)
	}
}

func TestClientGetUpdates(t *testing.T) {
	var gotBody map[string]any
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"chat":{"id":1,"type":"private"}}},{"update_id":11}]}`))
	})

	updates, err := client.GetUpdates(context.Background(), 10, 1)
	if err != nil {
		t.Fatalf("getUpdates не удался: %v", err)
	}
	if len(updates) != 2 || updates[0].UpdateID != 10 || updates[1].UpdateID != 11 {
		t.Fatalf("updates: %+v", updates)
	}
	if string(updates[0].Raw) == "" {
		t.Fatalf("raw update потерян")
	}
	if gotBody["offset"].(float64) != 10 || gotBody["timeout"].(float64) != 1 {
		t.Fatalf("payload: %v", gotBody)
	}
	if allowed, ok := gotBody["allowed_updates"].([]any); !ok || len(allowed) != 2 {
		t.Fatalf("allowed_updates: %v", gotBody["allowed_updates"])
	}
}

func TestClientGetUpdatesEmptyResult(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	updates, err := client.GetUpdates(context.Background(), 5, 1)
	if err != nil || len(updates) != 0 {
		t.Fatalf("пустой result: updates=%v err=%v", updates, err)
	}
}

func TestClient429HonorsRetryAfter(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`))
	})

	start := time.Now()
	_, err := client.SendMessage(context.Background(), 1, "x")
	if err == nil {
		t.Fatalf("429 должен давать ошибку")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ошибка не APIError: %v", err)
	}
	if apiErr.HTTPStatus != 429 {
		t.Fatalf("статус = %d", apiErr.HTTPStatus)
	}
	if apiErr.RetryAfter != 3*time.Second {
		t.Fatalf("retry_after = %v, хочу 3s", apiErr.RetryAfter)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("429 не должен ждать в клиенте — решает вызывающий (§13.2)")
	}
}

func TestClientConfirmedErrors(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"chat not found"}`))
	})
	_, err := client.SendMessage(context.Background(), 1, "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 400 || apiErr.RetryAfter != 0 {
		t.Fatalf("подтверждённая 400: %v", err)
	}
	if apiErr.Description != "chat not found" {
		t.Fatalf("description: %q", apiErr.Description)
	}
}

func TestClientOkFalseOnHTTP200(t *testing.T) {
	// Паритет TS: !response.ok || !ok — ошибка даже при HTTP 200.
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"internal reason"}`))
	})
	_, err := client.SendMessage(context.Background(), 1, "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 200 {
		t.Fatalf("ok=false при HTTP 200 должен быть ошибкой: %v", err)
	}
}

func TestClientCallTimeout(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})
	client.sendTimeout = 50 * time.Millisecond // таймаут на каждый вызов (§9.5)

	start := time.Now()
	_, err := client.SendMessage(context.Background(), 1, "x")
	if err == nil {
		t.Fatalf("таймаут должен давать ошибку")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("обрыв без ответа — не APIError (исход неизвестен, §13.2): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("таймаут не сработал: %v", elapsed)
	}
}

func TestClientChatActionAndReaction(t *testing.T) {
	var calls []string
	var bodies []map[string]any
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})

	if err := client.SendChatAction(context.Background(), 42, "typing"); err != nil {
		t.Fatalf("sendChatAction: %v", err)
	}
	if err := client.SetMessageReaction(context.Background(), 42, 777, "🔥"); err != nil {
		t.Fatalf("setMessageReaction: %v", err)
	}
	if len(calls) != 2 ||
		calls[0] != "/botTESTTOKEN/sendChatAction" ||
		calls[1] != "/botTESTTOKEN/setMessageReaction" {
		t.Fatalf("пути: %v", calls)
	}
	if bodies[0]["action"] != "typing" || bodies[1]["message_id"].(float64) != 777 {
		t.Fatalf("payload: %v", bodies)
	}
	// Контракт Bot API: reaction — список ReactionTypeEmoji (в JSON-теле —
	// настоящий массив). Параметр «emoji» Telegram молча игнорирует —
	// реакция «доставлена» по outbox и не видна в чате (находка приёмки Р).
	reactionList, ok := bodies[1]["reaction"].([]any)
	if !ok || len(reactionList) != 1 {
		t.Fatalf("payload реакции — список из одного: %v", bodies[1]["reaction"])
	}
	reaction, ok := reactionList[0].(map[string]any)
	if !ok || reaction["type"] != "emoji" || reaction["emoji"] != "🔥" {
		t.Fatalf("ReactionTypeEmoji: %v", reactionList[0])
	}
	if _, leak := bodies[1]["emoji"]; leak {
		t.Fatalf("параметр emoji не должен отправляться: %v", bodies[1])
	}
}

// TestClientErrorHidesToken — ошибки транспорта не светят токен в журнале:
// URL вида /bot<token>/… попадает в текст url.Error (находка приёмки Р:
// токен лежал в journald открытым текстом). errors.Is сохраняется —
// классификация доставки §13.2 различает таймаут и обрыв.
func TestClientErrorHidesToken(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})
	client.sendTimeout = 50 * time.Millisecond

	_, err := client.SendMessage(context.Background(), 1, "x")
	if err == nil {
		t.Fatal("таймаут должен давать ошибку")
	}
	if strings.Contains(err.Error(), "TESTTOKEN") {
		t.Fatalf("токен в тексте ошибки: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[токен]") {
		t.Fatalf("токен не помечен как скрытый: %q", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("классификация таймаута потеряна: %v", err)
	}
}

// TestDefaultTransportDialsTCP4 — дефолтный транспорт клиента набирает tcp4
// независимо от запрошенной сети (инцидент 2026-10-31: IPv6-флап провайдера —
// TCP устанавливался, данные висели в send-Q). Проверка: dial с явной сетью
// tcp6 на адрес 127.0.0.1 (IPv6 там невозможен) должен удаться — значит,
// сеть переписана на tcp4.
func TestDefaultTransportDialsTCP4(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	conn, err := defaultTransport().DialContext(context.Background(), "tcp6", server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("tcp4 не применился: %v", err)
	}
	_ = conn.Close()
}

func TestClientSendChatActionFitsCharacterPulse(t *testing.T) {
	// Контракт typing-пульса 9.4 (character.ChatActionSender) — структурно:
	// SendChatAction(ctx context.Context, chatID int64, action string) error.
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})
	var pulse interface {
		SendChatAction(ctx context.Context, chatID int64, action string) error
	} = client
	if err := pulse.SendChatAction(context.Background(), 1, "typing"); err != nil {
		t.Fatalf("пульс не смог использовать клиент: %v", err)
	}
}

func TestClientGetUpdatesLargeBatchBeyondMegabyte(t *testing.T) {
	// Регрессия приёмки 9.5: бёрст-пачка (сотни update'ов с длинными
	// кириллическими текстами) занимает больше 1 МиБ; обрезанное тело не
	// парсится и polling вечно повторял бы ту же пачку (§6.1) — getUpdates
	// обязан читать ответ целиком (паритет TS response.json()).
	const (
		updates = 100
		textLen = 12000 // символов кириллицы ≈ 24 КиБ UTF-8 на update
	)
	long := strings.Repeat("ж", textLen)
	items := make([]map[string]any, updates)
	for i := range items {
		items[i] = map[string]any{
			"update_id": 1000 + i,
			"message": map[string]any{
				"message_id": i + 1,
				"from":       map[string]any{"id": 42, "first_name": "Коля"},
				"chat":       map[string]any{"id": 42, "type": "private"},
				"text":       long,
			},
		}
	}
	payload, err := json.Marshal(map[string]any{"ok": true, "result": items})
	if err != nil {
		t.Fatalf("пачка не сериализуется: %v", err)
	}
	if len(payload) <= 1<<20 {
		t.Fatalf("тестовая пачка %d байт должна быть больше 1 МиБ", len(payload))
	}

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	got, err := client.GetUpdates(context.Background(), 0, 1)
	if err != nil {
		t.Fatalf("большая пачка не должна ломать getUpdates: %v", err)
	}
	if len(got) != updates || got[0].UpdateID != 1000 || got[updates-1].UpdateID != 1000+updates-1 {
		t.Fatalf("пачка прочитана не целиком: %d update'ов", len(got))
	}
}

// TestClientGetMe — M8: идентичность бота читается одним вызовом getMe.
func TestClientGetMe(t *testing.T) {
	var gotPath string
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":900,"username":"dio_bot","is_bot":true}}`))
	})
	bot, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatalf("getMe: %v", err)
	}
	if gotPath != "/botTESTTOKEN/getMe" {
		t.Fatalf("путь getMe = %q", gotPath)
	}
	if bot.ID != 900 || bot.Username != "dio_bot" {
		t.Fatalf("идентичность бота = %+v", bot)
	}
}
