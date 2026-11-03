// Тесты этапа 25.1–25.2 на стороне персонажа: какая картинка идёт модели из
// события пачки (фото, статичный стикер, превью анимированного/видео, кадр
// гифки) и как ход откатывается, если провайдер не принимает картинку.
package character

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestImageFileIDFromBatchEvent — приоритет источников картинки события:
// собственное фото, затем статичный стикер, затем превью стикера/гифки.
func TestImageFileIDFromBatchEvent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "фото",
			raw:  `{"message":{"photo":[{"file_id":"photo","width":10,"height":10}]}}`,
			want: "photo",
		},
		{
			name: "статичный стикер",
			raw:  `{"message":{"sticker":{"file_id":"sticker","file_unique_id":"u1","emoji":"😂"}}}`,
			want: "sticker",
		},
		{
			name: "анимированный стикер — превью",
			raw:  `{"message":{"sticker":{"file_id":"tgs","is_animated":true,"thumbnail":{"file_id":"tt"}}}}`,
			want: "tt",
		},
		{
			name: "видео-стикер — превью",
			raw:  `{"message":{"sticker":{"file_id":"webm","is_video":true,"thumb":{"file_id":"tv"}}}}`,
			want: "tv",
		},
		{
			name: "гифка — кадр",
			raw:  `{"message":{"animation":{"file_id":"gif","thumbnail":{"file_id":"gt"}}}}`,
			want: "gt",
		},
		{
			name: "голосовое — картинки нет",
			raw:  `{"message":{"voice":{"file_id":"v","duration":5}}}`,
			want: "",
		},
		{
			name: "текст — картинки нет",
			raw:  `{"message":{"text":"привет"}}`,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := BatchEvent{Raw: json.RawMessage(c.raw)}
			if got := imageFileIDFromBatchEvent(ev); got != c.want {
				t.Fatalf("картинка события = %q, хочу %q", got, c.want)
			}
		})
	}
}

// TestCollectImagesIncludesStickersAndGifs — в запрос модели попадают стикеры
// и гифки, а не только фото; потолок картинок на ход общий.
func TestCollectImagesIncludesStickersAndGifs(t *testing.T) {
	batch := []BatchEvent{
		{EventID: 1, Raw: json.RawMessage(`{"message":{"sticker":{"file_id":"s1","file_unique_id":"u1"}}}`)},
		{EventID: 2, Raw: json.RawMessage(`{"message":{"animation":{"file_id":"g1","thumbnail":{"file_id":"gt1"}}}}`)},
		{EventID: 3, Raw: json.RawMessage(`{"message":{"voice":{"file_id":"v","duration":3}}}`)},
		{EventID: 4, Raw: json.RawMessage(`{"message":{"sticker":{"file_id":"s2","is_animated":true,"thumbnail":{"file_id":"t2"}}}}`)},
		{EventID: 5, Raw: json.RawMessage(`{"message":{"photo":[{"file_id":"p1","width":9,"height":9}]}}`)},
	}
	var ids []string
	fetcher := &recordingPhotoFetcher{record: func(id string) { ids = append(ids, id) }}
	photos := CollectImages(context.Background(), batch, fetcher, 3, nil)
	if len(photos) != 3 {
		t.Fatalf("картинок %d, хочу 3", len(photos))
	}
	want := []string{"s1", "gt1", "t2"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("file_id картинок %v, хочу %v", ids, want)
		}
	}
}

// TestLooksLikeMediaRejection — откат на текст только при отказе именно по
// картинке: сетевая ошибка не должна превращаться в «ход без фото».
func TestLooksLikeMediaRejection(t *testing.T) {
	yes := []string{
		"400 Bad Request: unsupported image format image/webp",
		"invalid media type in image_url",
		"failed to decode image: image/webp not supported",
		`{"error":{"message":"Invalid image URL"}}`,
	}
	for _, s := range yes {
		if !looksLikeMediaRejection(errors.New(s)) {
			t.Fatalf("отказ по картинке не распознан: %q", s)
		}
	}
	no := []string{
		"context deadline exceeded",
		"429 too many requests",
		"connection refused",
	}
	for _, s := range no {
		if looksLikeMediaRejection(errors.New(s)) {
			t.Fatalf("обычная ошибка принята за отказ по картинке: %q", s)
		}
	}
	if looksLikeMediaRejection(nil) {
		t.Fatal("nil-ошибка не может быть отказом по картинке")
	}
}

// Тест ниже держит форму обещания: CollectImages не падает без фетчера.
func TestCollectImagesNilFetcher(t *testing.T) {
	var fetcher PhotoFetcher
	if got := CollectImages(context.Background(), []BatchEvent{{Raw: json.RawMessage(`{"message":{"text":"x"}}`)}}, fetcher, 3, nil); got != nil {
		t.Fatalf("без фетчера картинок быть не должно: %v", got)
	}
}
