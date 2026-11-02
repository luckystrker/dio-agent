package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPhotoFileIDFromUpdateLargest — берётся крупнейший размер по площади.
func TestPhotoFileIDFromUpdateLargest(t *testing.T) {
	raw := []byte(`{"update_id":1,"message":{"message_id":5,"date":1,
		"photo":[
			{"file_id":"small","width":90,"height":90},
			{"file_id":"","width":200,"height":200},
			{"file_id":"big","width":800,"height":600},
			{"file_id":"mid","width":320,"height":320}
		]}}`)
	if got := PhotoFileIDFromUpdate(raw); got != "big" {
		t.Fatalf("file_id = %q, хочу big", got)
	}
	if got := PhotoFileIDFromUpdate([]byte(`{"update_id":2}`)); got != "" {
		t.Fatalf("без фото — пусто, получено %q", got)
	}
}

// TestPhotoFileIDFromUpdateReplyToMessage — фото в reply_to_message доходит до
// модели (bug 2026-11-02): normalize пишет «[в ответ на X: (фото)]», но
// file_id брался только из собственного .photo сообщения — картинка терялась.
func TestPhotoFileIDFromUpdateReplyToMessage(t *testing.T) {
	raw := []byte(`{"message":{"text":"картинку то видишь?","reply_to_message":{"photo":[
		{"file_id":"a","width":40,"height":40},
		{"file_id":"b","width":570,"height":570}
	]}}}`)
	if got := PhotoFileIDFromUpdate(raw); got != "b" {
		t.Fatalf("file_id из reply_to_message = %q, хочу b", got)
	}
}

// TestPhotoFileIDFromUpdateOwnPhotoBeatsReply — собственное фото сообщения
// приоритетнее фото цитируемого: модель должна видеть то, что прислали сейчас.
func TestPhotoFileIDFromUpdateOwnPhotoBeatsReply(t *testing.T) {
	raw := []byte(`{"message":{
		"text":"смотри",
		"photo":[{"file_id":"own","width":20,"height":20}],
		"reply_to_message":{"photo":[{"file_id":"quoted","width":900,"height":900}]}
	}}`)
	if got := PhotoFileIDFromUpdate(raw); got != "own" {
		t.Fatalf("file_id = %q, хочу собственное фото own", got)
	}
}

// TestNormalizePhotoCaption — фото: kind=photo, текст = caption.
func TestNormalizePhotoCaption(t *testing.T) {
	raw := []byte(`{"update_id":3,"message":{"message_id":7,"date":1,
		"photo":[{"file_id":"f1","width":90,"height":90}],
		"caption":"смотри что снял",
		"from":{"id":777,"first_name":"Коля"},
		"chat":{"id":100,"type":"private"}}}`)
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if ev.Kind != KindPhoto {
		t.Fatalf("kind = %s, хочу photo", ev.Kind)
	}
	if ev.Text == nil || *ev.Text != "смотри что снял" {
		t.Fatalf("caption не стал текстом: %v", ev.Text)
	}
}

// TestFetchPhotoByFileID — getFile → скачивание → base64; лимит размера.
func TestFetchPhotoByFileID(t *testing.T) {
	const jpeg = "\xff\xd8\xff\xe0fakejpegbytes"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/getFile"):
			var body struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("body: %v", err)
			}
			if body.FileID != "fid-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"file_id": "fid-1", "file_path": "photos/file_1.jpg"},
			})
		case strings.Contains(r.URL.Path, "/file/"):
			if _, err := w.Write([]byte(jpeg)); err != nil {
				t.Errorf("write: %v", err)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewClient("TESTTOKEN", server.URL, nil)
	pm, err := c.FetchPhotoByFileID(context.Background(), "fid-1")
	if err != nil || pm == nil {
		t.Fatalf("fetch: %v %v", pm, err)
	}
	if pm.Mime != "image/jpeg" || pm.Base64 != "/9j/4GZha2VqcGVnYnl0ZXM=" {
		t.Fatalf("mime/base64: %q %q", pm.Mime, pm.Base64)
	}
	if string(pm.Data()) != jpeg {
		t.Fatal("Data() не совпадает с исходными байтами")
	}

	// Oversize → nil (ход по тексту).
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "x.jpg"}})
			return
		}
		_, _ = w.Write(make([]byte, MaxPhotoBytes+1))
	}))
	defer big.Close()
	pm2, err := NewClient("TESTTOKEN", big.URL, nil).FetchPhotoByFileID(context.Background(), "fid")
	if err != nil || pm2 != nil {
		t.Fatalf("oversize должен давать (nil, nil): %v %v", pm2, err)
	}
}
