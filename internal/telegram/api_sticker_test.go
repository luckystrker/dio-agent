// Тесты этапа 25.4: клиент Bot API отправляет стикер и возвращает message_id.
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSendSticker(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTESTTOKEN/sendSticker" {
			t.Errorf("путь = %q, хочу /botTESTTOKEN/sendSticker", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":321}}`))
	}))
	defer server.Close()

	id, err := NewClient("TESTTOKEN", server.URL, server.Client()).
		SendSticker(context.Background(), 42, "CAACAgIAAxkBAAIB")
	if err != nil || id != 321 {
		t.Fatalf("sendSticker: id=%d err=%v", id, err)
	}
	if body["chat_id"] != float64(42) || body["sticker"] != "CAACAgIAAxkBAAIB" {
		t.Fatalf("payload стикера: %v", body)
	}
	if _, ok := body["file_id"]; ok {
		t.Fatalf("стикер отправлен параметром file_id, а Bot API ждёт sticker: %v", body)
	}
}

// TestClientSendStickerWithoutMessageID — ответ без message_id — ошибка: без
// него outbox не зафиксирует отправку.
func TestClientSendStickerWithoutMessageID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	if _, err := NewClient("TESTTOKEN", server.URL, server.Client()).
		SendSticker(context.Background(), 1, "x"); err == nil {
		t.Fatal("ответ без message_id принят как успех")
	}
}
