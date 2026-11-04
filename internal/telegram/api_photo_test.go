// Тесты отправки картинки (этап 36.2): sendPhoto — multipart с файлом
// (Bot API принимает фото файлом, а не URL — URL модель не контролирует),
// ответ без message_id — подтверждённая ошибка метода.
package telegram

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendPhotoMultipart(t *testing.T) {
	var (
		gotChatID  string
		gotName    string
		gotType    string
		gotBytes   []byte
		gotPath    string
		gotMethod  string
		serverSeen bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverSeen = true
		gotPath = r.URL.Path
		gotMethod = r.Method
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("content-type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Errorf("content-type = %q (%v)", r.Header.Get("content-type"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("multipart не разобран: %v", err)
				return
			}
			switch part.FormName() {
			case "chat_id":
				raw, _ := io.ReadAll(part)
				gotChatID = string(raw)
			case "photo":
				gotName = part.FileName()
				gotType = part.Header.Get("content-type")
				gotBytes, _ = io.ReadAll(part)
			}
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":555}}`))
	}))
	defer srv.Close()

	client := NewClient("TESTTOKEN", srv.URL, srv.Client())
	image := []byte("\x89PNG\r\n\x1a\n fake-image")
	id, err := client.SendPhoto(context.Background(), 42, "photo.png", "image/png", image)
	if err != nil {
		t.Fatalf("sendPhoto не удался: %v", err)
	}
	if !serverSeen || gotPath != "/botTESTTOKEN/sendPhoto" || gotMethod != http.MethodPost {
		t.Fatalf("запрос не дошёл: path=%q method=%q", gotPath, gotMethod)
	}
	if id != 555 {
		t.Fatalf("message_id = %d, хочу 555", id)
	}
	if gotChatID != "42" || gotName != "photo.png" || gotType != "image/png" || !bytes.Equal(gotBytes, image) {
		t.Fatalf("multipart: chat_id=%q file=%q type=%q bytes=%q", gotChatID, gotName, gotType, gotBytes)
	}
}

func TestSendPhotoAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"PHOTO_INVALID_DIMENSIONS"}`))
	}))
	defer srv.Close()

	client := NewClient("TESTTOKEN", srv.URL, srv.Client())
	_, err := client.SendPhoto(context.Background(), 42, "photo.jpg", "image/jpeg", []byte("x"))
	if err == nil {
		t.Fatal("ошибка Bot API должна вернуться")
	}
}
