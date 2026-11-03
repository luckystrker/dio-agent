// Стикеры и гифки из raw_update (§6.1, этап 25.1–25.2). Нормализация даёт
// текстом эмодзи/«[гифка]», а картинку модель получает отдельной частью Ask:
// статичный стикер — сам webp-файл, анимированный (.tgs) и видео (.webm) —
// превью-кадр (thumbnail), гифка — кадр-превью. Своё сообщение приоритетно:
// чужие вложения цитируемого reply картинкой не тянем (цитата идёт текстом).
package telegram

import "encoding/json"

// StickerInfo — то, что runtime знает о стикере пачки: file_id для отправки
// (25.4), file_unique_id для дедупликации коллекции (25.3/25.5), эмодзи и
// набор для панели, тип — для выбора «картинка или только эмодзи».
type StickerInfo struct {
	FileID          string
	FileUniqueID    string
	Emoji           string
	SetName         string
	Animated        bool
	Video           bool
	ThumbnailFileID string
}

// ImageFileID — file_id картинки для модели: статичный стикер целиком, для
// анимированного/видео — превью. Пусто — картинки нет (модель видит эмодзи).
func (s *StickerInfo) ImageFileID() string {
	if s == nil {
		return ""
	}
	if !s.Animated && !s.Video {
		return s.FileID
	}
	return s.ThumbnailFileID
}

// StickerFromUpdate — стикер собственного сообщения update; nil — не стикер.
func StickerFromUpdate(raw []byte) *StickerInfo {
	message := messageFromUpdate(raw)
	if message == nil || message.Sticker == nil {
		return nil
	}
	sticker := message.Sticker
	return &StickerInfo{
		FileID:          sticker.FileID,
		FileUniqueID:    sticker.FileUniqueID,
		Emoji:           sticker.Emoji,
		SetName:         sticker.SetName,
		Animated:        sticker.IsAnimated,
		Video:           sticker.IsVideo,
		ThumbnailFileID: sticker.previewFileID(),
	}
}

// AnimationThumbnailFileID — file_id кадра-превью гифки; пусто — превью нет
// (собственный файл .mp4 картинкой модели не отдаём).
func AnimationThumbnailFileID(raw []byte) string {
	message := messageFromUpdate(raw)
	if message == nil || message.Animation == nil {
		return ""
	}
	return message.Animation.previewFileID()
}

// messageFromUpdate — сообщение update (своё/правка/пост канала), как в
// нормализации; разбор не удался — nil (стикера нет, ход продолжается).
func messageFromUpdate(raw []byte) *telegramMessage {
	var shape telegramUpdateShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil
	}
	message, _ := extractMessage(&shape)
	return message
}
