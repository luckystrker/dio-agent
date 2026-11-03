// Тесты этапа 25 (стикеры, гифки): что runtime берёт из raw_update и каким
// текстом событие входит в ход/историю. Статичный стикер — webp-картинка,
// анимированный/видео — эмодзи + превью (thumbnail), гифка — превью-кадр.
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStickerFromUpdateStatic — статичный стикер: картинкой идёт сам файл,
// эмодзи/набор/уникальный id доступны runtime (коллекция стикеров, 25.3).
func TestStickerFromUpdateStatic(t *testing.T) {
	raw := []byte(`{"message":{"message_id":1,"sticker":{
		"file_id":"S1","file_unique_id":"U1","emoji":"😂","set_name":"dio_pack",
		"is_animated":false,"is_video":false,
		"thumbnail":{"file_id":"T1","width":64,"height":64}}}}`)
	info := StickerFromUpdate(raw)
	if info == nil {
		t.Fatal("стикер не распознан")
	}
	if info.FileID != "S1" || info.FileUniqueID != "U1" || info.Emoji != "😂" || info.SetName != "dio_pack" {
		t.Fatalf("поля стикера: %+v", info)
	}
	if info.Animated || info.Video {
		t.Fatalf("статичный стикер помечен анимированным/видео: %+v", info)
	}
	if got := info.ImageFileID(); got != "S1" {
		t.Fatalf("картинка статичного стикера = %q, хочу file_id S1", got)
	}
}

// TestStickerFromUpdateAnimatedAndVideo — .tgs/.webm картинкой модели не
// отдаются: рисуем превью (thumbnail, Bot API 6.6) и старый thumb.
func TestStickerFromUpdateAnimatedAndVideo(t *testing.T) {
	animated := []byte(`{"message":{"sticker":{"file_id":"A1","file_unique_id":"AU1",
		"emoji":"🔥","is_animated":true,"thumbnail":{"file_id":"TA"}}}}`)
	info := StickerFromUpdate(animated)
	if info == nil || !info.Animated {
		t.Fatalf("анимированный стикер не распознан: %+v", info)
	}
	if got := info.ImageFileID(); got != "TA" {
		t.Fatalf("картинка анимированного = %q, хочу превью TA", got)
	}

	video := []byte(`{"message":{"sticker":{"file_id":"V1","file_unique_id":"VU1",
		"is_video":true,"thumb":{"file_id":"TV"}}}}`)
	info = StickerFromUpdate(video)
	if info == nil || !info.Video {
		t.Fatalf("видео-стикер не распознан: %+v", info)
	}
	if got := info.ImageFileID(); got != "TV" {
		t.Fatalf("картинка видео-стикера = %q, хочу устаревшее поле thumb TV", got)
	}

	// Без превью картинки нет: модель видит только эмодзи (честный фолбэк).
	bare := []byte(`{"message":{"sticker":{"file_id":"V2","file_unique_id":"VU2","is_video":true}}}`)
	if got := StickerFromUpdate(bare).ImageFileID(); got != "" {
		t.Fatalf("без превью картинки быть не должно, получено %q", got)
	}
}

// TestStickerFromUpdateAbsent — не стикер: nil (фото/текст не ломаются).
func TestStickerFromUpdateAbsent(t *testing.T) {
	for _, raw := range []string{
		`{"message":{"text":"привет"}}`,
		`{"message":{"photo":[{"file_id":"f","width":10,"height":10}]}}`,
		`not json`,
	} {
		if info := StickerFromUpdate([]byte(raw)); info != nil {
			t.Fatalf("не стикер, а распознан: %q → %+v", raw, info)
		}
	}
}

// TestAnimationThumbnailFileID — гифка (animation): модели отдаётся кадр-превью,
// собственный файл (.mp4) картинкой не является.
func TestAnimationThumbnailFileID(t *testing.T) {
	raw := []byte(`{"message":{"animation":{"file_id":"G1","file_unique_id":"GU1",
		"duration":2,"width":320,"height":180,"thumbnail":{"file_id":"GT"}},"caption":"кот"}}`)
	if got := AnimationThumbnailFileID(raw); got != "GT" {
		t.Fatalf("превью гифки = %q, хочу GT", got)
	}
	if got := AnimationThumbnailFileID([]byte(`{"message":{"animation":{"file_id":"G2"}}}`)); got != "" {
		t.Fatalf("без превью ожидалась пустая строка, получено %q", got)
	}
	if got := AnimationThumbnailFileID([]byte(`{"message":{"text":"x"}}`)); got != "" {
		t.Fatalf("не гифка, получено %q", got)
	}
}

// TestNormalizeAnimation — текст события «[гифка]», подпись приклеивается.
func TestNormalizeAnimation(t *testing.T) {
	base := `{"update_id":1,"message":{"message_id":1,"date":1,
		"animation":{"file_id":"g","duration":2},
		"from":{"id":777,"first_name":"Коля"},"chat":{"id":100,"type":"private"}`
	ev, err := NormalizeUpdate([]byte(base + `}}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if ev.Text == nil || *ev.Text != "[гифка]" {
		t.Fatalf("текст гифки = %v, хочу «[гифка]»", ev.Text)
	}

	withCaption := base + `,"caption":"ржака"}}`
	ev, err = NormalizeUpdate([]byte(withCaption))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if ev.Text == nil || *ev.Text != "[гифка] ржака" {
		t.Fatalf("текст гифки с подписью = %v, хочу «[гифка] ржака»", ev.Text)
	}
}

// TestNormalizeVoiceAndVideoNote — голосовые и кружки: длительность в тексте
// события (25.6), транскрипции нет.
func TestNormalizeVoiceAndVideoNote(t *testing.T) {
	cases := []struct {
		field string
		want  string
	}{
		{`"voice":{"file_id":"v","duration":42}`, "[голосовое, 0:42]"},
		{`"voice":{"file_id":"v","duration":125}`, "[голосовое, 2:05]"},
		{`"video_note":{"file_id":"v","duration":15}`, "[кружок, 0:15]"},
	}
	for _, c := range cases {
		raw := []byte(`{"update_id":1,"message":{"message_id":1,"date":1,` + c.field + `,
			"from":{"id":1,"first_name":"К"},"chat":{"id":100,"type":"private"}}}`)
		ev, err := NormalizeUpdate(raw)
		if err != nil {
			t.Fatalf("%s: normalize: %v", c.field, err)
		}
		if ev.Text == nil || *ev.Text != c.want {
			t.Fatalf("%s: текст = %v, хочу %q", c.field, ev.Text, c.want)
		}
	}
}

// TestFetchStickerWebpMime — тип картинки берётся из расширения файла, а не
// жёстко image/jpeg: webp-стикер уходил модели как jpeg (25.1).
func TestFetchStickerWebpMime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "result": map[string]any{"file_path": "stickers/file_1.webp"},
			})
			return
		}
		_, _ = w.Write([]byte("RIFF....WEBP"))
	}))
	defer server.Close()

	pm, err := NewClient("TESTTOKEN", server.URL, nil).FetchPhotoByFileID(context.Background(), "sticker")
	if err != nil || pm == nil {
		t.Fatalf("fetch: %v %v", pm, err)
	}
	if pm.Mime != "image/webp" {
		t.Fatalf("mime стикера = %q, хочу image/webp", pm.Mime)
	}
}
