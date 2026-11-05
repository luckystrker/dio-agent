// Тесты реплая и пересылки (этапы 37.2–37.3) через httptest Bot API:
// reply_parameters с allow_sending_without_reply и forwardMessage только из
// текущего чата в него же.
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSendMessageReply(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTESTTOKEN/sendMessage" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":778}}`))
	}))
	defer server.Close()

	id, err := NewClient("TESTTOKEN", server.URL, server.Client()).
		SendMessage(context.Background(), 42, "ответ реплаем", 9001)
	if err != nil || id != 778 {
		t.Fatalf("sendMessage с реплаем: id=%d err=%v", id, err)
	}
	params, ok := body["reply_parameters"].(map[string]any)
	if !ok {
		t.Fatalf("reply_parameters не отправлены: %v", body)
	}
	if params["message_id"] != float64(9001) {
		t.Fatalf("reply_parameters.message_id = %v", params["message_id"])
	}
	if params["allow_sending_without_reply"] != true {
		t.Fatalf("allow_sending_without_reply должен быть true: %v", params)
	}
}

func TestClientSendMessageWithoutReplyOmitsParameters(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	if _, err := NewClient("TESTTOKEN", server.URL, server.Client()).
		SendMessage(context.Background(), 42, "обычное", 0); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if _, ok := body["reply_parameters"]; ok {
		t.Fatalf("reply_parameters без реплая: %v", body)
	}
}

func TestClientForwardMessage(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTESTTOKEN/forwardMessage" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":779}}`))
	}))
	defer server.Close()

	id, err := NewClient("TESTTOKEN", server.URL, server.Client()).
		ForwardMessage(context.Background(), 42, 42, 1234)
	if err != nil || id != 779 {
		t.Fatalf("forwardMessage: id=%d err=%v", id, err)
	}
	if body["chat_id"] != float64(42) || body["from_chat_id"] != float64(42) || body["message_id"] != float64(1234) {
		t.Fatalf("payload пересылки: %v", body)
	}
}

func TestClientForwardMessageWithoutID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	if _, err := NewClient("TESTTOKEN", server.URL, server.Client()).
		ForwardMessage(context.Background(), 1, 1, 2); err == nil {
		t.Fatal("ответ без message_id принят как успех")
	}
}
