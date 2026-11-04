// Тесты этапа 35 на стороне Telegram: разбор видео из update, пометка
// раскадровки в тексте события и скачивание видео с потолком размера.
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVideoFromUpdate — гифка и кружок дают file_id, длительность и превью;
// голосовое/текст видео не считаются (их раскадровка не касается).
func TestVideoFromUpdate(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *VideoInfo
	}{
		{
			name: "гифка",
			raw:  `{"message":{"animation":{"file_id":"gif","duration":12,"thumbnail":{"file_id":"gt"}}}}`,
			want: &VideoInfo{FileID: "gif", Duration: 12, Kind: VideoKindAnimation, ThumbnailFileID: "gt"},
		},
		{
			name: "кружок с превью",
			raw:  `{"message":{"video_note":{"file_id":"note","duration":15,"thumbnail":{"file_id":"nt"}}}}`,
			want: &VideoInfo{FileID: "note", Duration: 15, Kind: VideoKindVideoNote, ThumbnailFileID: "nt"},
		},
		{
			name: "кружок без превью",
			raw:  `{"message":{"video_note":{"file_id":"note","duration":15}}}`,
			want: &VideoInfo{FileID: "note", Duration: 15, Kind: VideoKindVideoNote},
		},
		{
			name: "голосовое — не видео",
			raw:  `{"message":{"voice":{"file_id":"v","duration":5}}}`,
			want: nil,
		},
		{
			name: "текст — не видео",
			raw:  `{"message":{"text":"привет"}}`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VideoFromUpdate([]byte(c.raw))
			if c.want == nil {
				if got != nil {
					t.Fatalf("видео найдено ошибочно: %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("видео не найдено")
			}
			if got.FileID != c.want.FileID || got.Duration != c.want.Duration ||
				got.Kind != c.want.Kind || got.ThumbnailFileID != c.want.ThumbnailFileID {
				t.Fatalf("разбор = %+v, хочу %+v", got, c.want)
			}
		})
	}
}

// TestAnnotateStoryboard — текст события говорит модели, что пришли кадры;
// у кружка честно приписано, что звук не слышен. Пометка не ломает цитату
// реплая и подпись гифки.
func TestAnnotateStoryboard(t *testing.T) {
	gif := &VideoInfo{Kind: VideoKindAnimation, Duration: 8}
	note := &VideoInfo{Kind: VideoKindVideoNote, Duration: 15}
	cases := []struct {
		name   string
		text   string
		info   *VideoInfo
		frames int
		want   string
	}{
		{
			name:   "гифка с подписью",
			text:   "[в ответ на Вася: «привет»] [гифка] смотри",
			info:   gif,
			frames: 5,
			want:   "[в ответ на Вася: «привет»] [гифка, раскадровка 5 кадров] смотри",
		},
		{
			name:   "кружок",
			text:   "[кружок, 0:15]",
			info:   note,
			frames: 4,
			want:   "[кружок, 0:15, раскадровка 4 кадра — звук не слышишь]",
		},
		{
			name:   "один кадр",
			text:   "[кружок, 0:03]",
			info:   &VideoInfo{Kind: VideoKindVideoNote, Duration: 3},
			frames: 1,
			want:   "[кружок, 0:03, раскадровка 1 кадр — звук не слышишь]",
		},
		{
			name:   "без кадров — текст не меняется",
			text:   "[кружок, 0:15]",
			info:   note,
			frames: 0,
			want:   "[кружок, 0:15]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AnnotateStoryboard(c.text, c.info, c.frames); got != c.want {
				t.Fatalf("текст = %q, хочу %q", got, c.want)
			}
		})
	}
}

// TestFetchVideoByFileID — видео скачивается по getFile; file_size больше
// потолка отсекается до загрузки (nil, nil — ход идёт по превью).
func TestFetchVideoByFileID(t *testing.T) {
	const mp4 = "fake-mp4-bytes"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/getFile"):
			var body struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("body: %v", err)
			}
			result := map[string]any{"file_id": body.FileID, "file_path": "videos/file_1.mp4"}
			if body.FileID == "too-big" {
				result["file_size"] = MaxVideoBytes + 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
		case strings.Contains(r.URL.Path, "/file/"):
			if r.URL.EscapedPath() != "/file/botTESTTOKEN/videos/file_1.mp4" {
				t.Errorf("путь скачивания %q", r.URL.EscapedPath())
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(mp4))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewClient("TESTTOKEN", server.URL, nil)
	vm, err := c.FetchVideoByFileID(context.Background(), "vid-1")
	if err != nil || vm == nil {
		t.Fatalf("fetch: %v %v", vm, err)
	}
	if string(vm.Data) != mp4 {
		t.Fatalf("байты = %q, хочу %q", vm.Data, mp4)
	}
	if big, err := c.FetchVideoByFileID(context.Background(), "too-big"); err != nil || big != nil {
		t.Fatalf("слишком большой файл должен отсекаться: %v %v", big, err)
	}
}
