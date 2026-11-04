// Юнит-тесты Bot API для аватарок (34.2): getUserProfilePhotos (limit 1,
// крупнейший размер), пустой ответ, скачивание через getFile. httptest.
package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientGetUserProfilePhotos(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"total_count":1,"photos":[[
			{"file_id":"small","file_unique_id":"u-small","width":64,"height":64},
			{"file_id":"big","file_unique_id":"u-big","width":320,"height":320}
		]]}}`))
	})

	photo, err := client.GetUserProfilePhotos(context.Background(), 777)
	if err != nil {
		t.Fatalf("getUserProfilePhotos не удался: %v", err)
	}
	if gotPath != "/botTESTTOKEN/getUserProfilePhotos" {
		t.Fatalf("путь = %s", gotPath)
	}
	if gotBody["user_id"].(float64) != 777 || gotBody["limit"].(float64) != 1 {
		t.Fatalf("payload: %v", gotBody)
	}
	if photo.FileID != "big" || photo.FileUniqueID != "u-big" {
		t.Fatalf("выбран не крупнейший размер: %+v", photo)
	}
}

func TestClientGetUserProfilePhotosEmpty(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"total_count":0,"photos":[]}}`))
	})
	photo, err := client.GetUserProfilePhotos(context.Background(), 1)
	if err != nil {
		t.Fatalf("пустой ответ — не ошибка: %v", err)
	}
	if photo.FileID != "" {
		t.Fatalf("аватарки нет: %+v", photo)
	}
}

func TestAvatarFetcherDownloadsPhoto(t *testing.T) {
	const payload = "avatar-bytes"
	var filePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTESTTOKEN/getFile":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body["file_id"] != "big" {
				t.Errorf("getFile file_id = %v", body["file_id"])
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"big","file_path":"photos/avatar.jpg"}}`))
		case "/file/botTESTTOKEN/photos/avatar.jpg":
			filePath = r.URL.Path
			_, _ = w.Write([]byte(payload))
		default:
			t.Errorf("неожиданный путь: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	fetcher := &AvatarFetcher{client: NewClient("TESTTOKEN", server.URL, server.Client())}
	mime, data, err := fetcher.FetchAvatar(context.Background(), "big")
	if err != nil {
		t.Fatalf("аватарка не скачана: %v", err)
	}
	if string(data) != payload || mime != "image/jpeg" {
		t.Fatalf("данные/MIME: %q %q", data, mime)
	}
	if filePath != "/file/botTESTTOKEN/photos/avatar.jpg" {
		t.Fatalf("путь скачивания = %q", filePath)
	}
}
