// Картинки пачки для модели (этап 25.1–25.2): фото, статичные стикеры, превью
// анимированных/видео-стикеров, кадр гифки и превью кружка (раскадровка видео —
// storyboard.go, этап 35). Всё это — части Ask (events.ImageInput); смысл
// вложения модель знает из текста события («[стикер 😂]», «[гифка]»,
// «[кружок, 0:15]», «[голосовое, 0:42]»).
package character

import (
	"context"
	"strings"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// imageFileIDFromBatchEvent — какая картинка идёт модели из события пачки:
// собственное фото, затем статичный стикер, затем превью стикера или гифки.
// Пусто — картинки нет (текст события остаётся).
func imageFileIDFromBatchEvent(ev BatchEvent) string {
	if len(ev.Raw) == 0 {
		return ""
	}
	if fileID := telegram.PhotoFileIDFromUpdate(ev.Raw); fileID != "" {
		return fileID
	}
	if sticker := telegram.StickerFromUpdate(ev.Raw); sticker != nil {
		return sticker.ImageFileID()
	}
	// Гифка и кружок: без раскадровки модели идёт превью-кадр (этап 35).
	if video := telegram.VideoFromUpdate(ev.Raw); video != nil {
		return video.ThumbnailFileID
	}
	return ""
}

// CollectImages собирает до max картинок пачки: фото, стикеры, превью гифок.
// Ошибки скачивания не роняют ход (событие остаётся текстом), причина — в logf.
func CollectImages(ctx context.Context, batch []BatchEvent, fetcher PhotoFetcher, max int, logf func(format string, args ...any)) []Photo {
	return collectImages(ctx, batch, fetcher, max, imageFileIDFromBatchEvent, logf)
}

// mediaRejectionMarkers — признаки отказа провайдера именно по картинке:
// только тогда ход честно откатывается на текст (стикеры/гифки остаются
// эмодзи и пометкой). Сеть, бюджет и прочие сбои сюда не попадают.
var mediaRejectionMarkers = []string{
	"image", "media type", "media_type", "mime", "webp",
}

// looksLikeMediaRejection — ошибка модели похожа на «картинку не принимаю».
// Строки провайдеров живут в тексте ошибки (OpenAI-совместимые отдают
// «invalid image», «unsupported media type» и т.п.), а класс ошибки ago
// не типизирован по причине — поэтому проверка по тексту.
func looksLikeMediaRejection(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range mediaRejectionMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
