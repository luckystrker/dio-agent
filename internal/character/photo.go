// Фото из Telegram → модель (vision, §6.1/§17): file_id из raw_update пачки,
// скачивание делает telegram-фетчер, в запуск агента уходят части
// [текст, image...] через events.ImageInput (ago рендерит image_url).
// Портировано из TS-линии da9559d, 2026-10-31.
package character

import (
	"context"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// Photo — скачанное фото одного события пачки.
type Photo struct {
	FileID string
	Data   []byte
	Mime   string
}

// PhotoFetcher — скачивание фото по file_id; nil/ошибка — ход по тексту.
// Реализация в internal/telegram, потребление в Runner (интерфейс здесь).
type PhotoFetcher interface {
	FetchPhotoByFileID(ctx context.Context, fileID string) (*telegram.PhotoMedia, error)
}

// photoFileIDFromBatchEvent — file_id из raw_update события пачки.
func photoFileIDFromBatchEvent(ev BatchEvent) string {
	if len(ev.Raw) == 0 {
		return ""
	}
	return telegram.PhotoFileIDFromUpdate(ev.Raw)
}

// CollectPhotos собирает до max фото пачки (TS: MAX_IMAGES_PER_TURN).
// Ошибки скачивания не роняют ход: фото пропускается, текст остаётся.
func CollectPhotos(ctx context.Context, batch []BatchEvent, fetcher PhotoFetcher, max int) []Photo {
	if fetcher == nil || len(batch) == 0 {
		return nil
	}
	if max <= 0 {
		max = telegram.MaxImagesPerTurn
	}
	var photos []Photo
	for _, ev := range batch {
		if len(photos) >= max {
			break
		}
		fileID := photoFileIDFromBatchEvent(ev)
		if fileID == "" {
			continue
		}
		pm, err := fetcher.FetchPhotoByFileID(ctx, fileID)
		if err != nil || pm == nil {
			continue
		}
		photos = append(photos, Photo{FileID: fileID, Data: pm.Data(), Mime: pm.Mime})
	}
	return photos
}

// askMsgs — аргументы Ask: текст пачки + фото как BinaryInput (ago рендерит
// их в image_url с data-URL, config/openaicompat.go).
func askMsgs(userText string, photos []Photo) []any {
	msgs := []any{userText}
	for _, p := range photos {
		input, err := events.ImageInput("", "", p.Data, p.Mime, "") // ровно один источник: байты
		if err != nil {
			continue
		}
		msgs = append(msgs, input)
	}
	return msgs
}
