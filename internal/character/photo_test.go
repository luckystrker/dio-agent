package character

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

type fakePhotoFetcher struct {
	calls int
	media *telegram.PhotoMedia
	err   error
}

func (f *fakePhotoFetcher) FetchPhotoByFileID(_ context.Context, _ string) (*telegram.PhotoMedia, error) {
	f.calls++
	return f.media, f.err
}

// TestCollectPhotosCapsAndTolerates — до max фото, ошибки скачивания не роняют
// ход, nil-фетчер — фото нет.
func TestCollectPhotosCapsAndTolerates(t *testing.T) {
	raw := func(id string) json.RawMessage {
		return json.RawMessage(`{"message":{"photo":[{"file_id":"` + id + `","width":10,"height":10}]}}`)
	}
	batch := []BatchEvent{
		{EventID: 1, Text: "раз", Raw: raw("a")},
		{EventID: 2, Raw: raw("b")},
		{EventID: 3, Raw: raw("c")},
		{EventID: 4, Raw: raw("d")}, // за лимитом 3
		{EventID: 5, Text: "просто текст"},
	}

	fetcher := &fakePhotoFetcher{media: &telegram.PhotoMedia{Mime: "image/jpeg", Base64: "eHk="}}
	photos := CollectPhotos(context.Background(), batch, fetcher, 3)
	if len(photos) != 3 || fetcher.calls != 3 {
		t.Fatalf("фото %d (вызовов %d), хочу 3/3", len(photos), fetcher.calls)
	}

	// Ошибка фетчера: фото пропускается, ход не роняется.
	failing := &fakePhotoFetcher{err: context.DeadlineExceeded}
	if got := CollectPhotos(context.Background(), batch[:1], failing, 3); len(got) != 0 {
		t.Fatalf("при ошибке фетчера фото быть не должно: %v", got)
	}
	if CollectPhotos(context.Background(), batch, nil, 3) != nil {
		t.Fatal("nil-фетчер — фото нет")
	}
}

// TestCollectPhotosFromReply — фото цитируемого сообщения коллекционируется:
// сообщение с текстом и без собственного .photo не должно оставлять модель
// без картинки (bug 2026-11-02).
func TestCollectPhotosFromReply(t *testing.T) {
	batch := []BatchEvent{{
		EventID: 1,
		Text:    "картинку то видишь?",
		Raw: json.RawMessage(`{"message":{"text":"картинку то видишь?",
			"reply_to_message":{"photo":[
				{"file_id":"a","width":40,"height":40},
				{"file_id":"b","width":570,"height":570}
			]}}}`),
	}}
	var gotID string
	fetcher := &recordingPhotoFetcher{record: func(id string) { gotID = id }}
	photos := CollectPhotos(context.Background(), batch, fetcher, 3)
	if len(photos) != 1 || gotID != "b" {
		t.Fatalf("фото из reply: собрано %d, file_id %q; хочу 1/b", len(photos), gotID)
	}
}

type recordingPhotoFetcher struct {
	record func(string)
}

func (f *recordingPhotoFetcher) FetchPhotoByFileID(_ context.Context, fileID string) (*telegram.PhotoMedia, error) {
	f.record(fileID)
	return &telegram.PhotoMedia{Mime: "image/jpeg", Base64: "eHk="}, nil
}

// TestAskMsgs — части Ask: текст + BinaryInput-фото (ago рендерит image_url).
func TestAskMsgs(t *testing.T) {
	msgs := askMsgs("смотри что снял", []Photo{
		{FileID: "f1", Data: []byte("abc"), Mime: "image/jpeg"},
	})
	if len(msgs) != 2 {
		t.Fatalf("частей %d, хочу 2 (текст+фото)", len(msgs))
	}
	if text, ok := msgs[0].(string); !ok || text != "смотри что снял" {
		t.Fatalf("первая часть — текст пачки: %T %v", msgs[0], msgs[0])
	}
	input, ok := msgs[1].(events.Input)
	if !ok {
		t.Fatalf("вторая часть не events.Input: %T", msgs[1])
	}
	if bin, ok := input.(*events.BinaryInput); !ok ||
		bin.Kind != events.BinaryImage || bin.MediaType != "image/jpeg" || string(bin.Data) != "abc" {
		t.Fatalf("фото-часть неверна: %+v", input)
	}
	// Без фото — просто текст.
	if msgs := askMsgs("текст", nil); len(msgs) != 1 {
		t.Fatalf("без фото — одна текстовая часть, стало %d", len(msgs))
	}
}
