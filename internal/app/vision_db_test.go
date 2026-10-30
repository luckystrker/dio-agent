//go:build db

// Vision-фото сквозь приложение (§6.1, портировано из TS-линии da9559d):
// фото-апдейт → ingest → ход с caption → фетчер скачал file_id → ответ доставлен.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// fakePhotoFetcher — программный фетчер: считает вызовы, отдаёт фиксированное фото.
type fakePhotoFetcher struct {
	calls   int
	fileIDs []string
}

func (f *fakePhotoFetcher) FetchPhotoByFileID(_ context.Context, fileID string) (*telegram.PhotoMedia, error) {
	f.calls++
	f.fileIDs = append(f.fileIDs, fileID)
	return &telegram.PhotoMedia{Mime: "image/jpeg", Base64: "aGVsbG8="}, nil
}

// pushPhoto — фото-сообщение с подписью и file_id (raw содержит photo-массив).
func (s *stubAPI) pushPhoto(updateID, chatID int64, caption, fileID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgID++
	raw := fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":%d,"date":1,"caption":%q,
		"photo":[{"file_id":"thumb","width":90,"height":90},{"file_id":%q,"width":1280,"height":720}],
		"from":{"id":777,"first_name":"Коля"},"chat":{"id":%d,"type":"private"}}}`,
		updateID, s.msgID, caption, fileID, chatID)
	s.updates = append(s.updates, telegram.Update{UpdateID: updateID, Raw: json.RawMessage(raw)})
}

func TestVisionPhotoTurn(t *testing.T) {
	fetcher := &fakePhotoFetcher{}
	h := newHarnessTuned(t, func(o *Options) { o.PhotoFetcher = fetcher },
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":["вижу закат, красиво"],"reason":"фото с caption"}`}})

	// Фото с подписью в личке.
	h.api.pushPhoto(1, 100, "смотри какой закат", "fid-777")
	eventually(t, "ход по фото отвечен", func() bool {
		return len(h.api.callsOf("sendMessage")) >= 1
	})
	if fetcher.calls != 1 || len(fetcher.fileIDs) != 1 || fetcher.fileIDs[0] != "fid-777" {
		t.Fatalf("фетчер должен скачаться один раз по fid-777 (крупнейший размер): %d %v", fetcher.calls, fetcher.fileIDs)
	}
	if got := h.api.callsOf("sendMessage")[0].Text; !strings.Contains(got, "закат") {
		t.Fatalf("ответ не про фото: %q", got)
	}
}
