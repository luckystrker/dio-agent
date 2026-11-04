// Видео Telegram (этап 35): гифка (animation, mp4) и кружок (video_note, mp4)
// целиком скачиваются для раскадровки. Потолок 20 МиБ — предел Bot API getFile;
// больше — превью-кадр, без ошибки хода.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MaxVideoBytes — потолок скачивания видео: больше не тянем в контекст и
// ffmpeg (Bot API getFile и так ограничен 20 МиБ).
const MaxVideoBytes = 20 << 20

// VideoKind — что за видео: гифка или кружок. Вид нужен пометке события.
type VideoKind string

const (
	VideoKindAnimation VideoKind = "animation"
	VideoKindVideoNote VideoKind = "video_note"
)

// VideoInfo — видео собственного сообщения update для раскадровки.
type VideoInfo struct {
	FileID          string
	FileUniqueID    string
	Duration        int
	Kind            VideoKind
	ThumbnailFileID string
}

// VideoMedia — скачанные байты видео.
type VideoMedia struct {
	Data []byte
}

// VideoFromUpdate — гифка или кружок собственного сообщения; nil — видео нет.
// Цитируемый reply видео не отдаёт: картинка цитаты идёт текстом.
func VideoFromUpdate(raw []byte) *VideoInfo {
	message := messageFromUpdate(raw)
	if message == nil {
		return nil
	}
	if message.Animation != nil && message.Animation.FileID != "" {
		return &VideoInfo{
			FileID:          message.Animation.FileID,
			FileUniqueID:    message.Animation.FileUniqueID,
			Duration:        message.Animation.Duration,
			Kind:            VideoKindAnimation,
			ThumbnailFileID: message.Animation.previewFileID(),
		}
	}
	if message.VideoNote != nil && message.VideoNote.FileID != "" {
		return &VideoInfo{
			FileID:          message.VideoNote.FileID,
			Duration:        message.VideoNote.Duration,
			Kind:            VideoKindVideoNote,
			ThumbnailFileID: message.VideoNote.previewFileID(),
		}
	}
	return nil
}

// AnnotateStoryboard — текст события с пометкой раскадровки: модель должна
// знать, что кадры пришли вложением, и что звук кружка не слышен. frames <= 0 —
// текст не меняется (раскадровки не было, работает превью-кадр).
func AnnotateStoryboard(text string, info *VideoInfo, frames int) string {
	if frames <= 0 || info == nil || text == "" {
		return text
	}
	plural := "кадров"
	switch {
	case frames%10 == 1 && frames%100 != 11:
		plural = "кадр"
	case frames%10 >= 2 && frames%10 <= 4 && (frames%100 < 10 || frames%100 >= 20):
		plural = "кадра"
	}
	marker := fmt.Sprintf("раскадровка %d %s", frames, plural)

	switch info.Kind {
	case VideoKindAnimation:
		// Гифка: подпись остаётся после метки, цитата реплая — до неё.
		old := "[гифка"
		if !strings.Contains(text, old) {
			break
		}
		return strings.Replace(text, old, "[гифка, "+marker, 1)
	case VideoKindVideoNote:
		old := "[кружок, " + formatMediaDuration(info.Duration) + "]"
		if strings.Contains(text, old) {
			return strings.Replace(text, old,
				"[кружок, "+formatMediaDuration(info.Duration)+", "+marker+" — звук не слышишь]", 1)
		}
	}
	// Неожиданный формат текста — пометку всё равно не теряем.
	return text + " [" + marker + "]"
}

// FetchVideoByFileID качает видео по file_id: getFile → file_path → байты.
// Слишком большой файл/пустой — (nil, nil): ход идёт по превью-кадру.
func (c *Client) FetchVideoByFileID(ctx context.Context, fileID string) (*VideoMedia, error) {
	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	raw, err := c.call(gctx, "getFile", map[string]string{"file_id": fileID}, 1<<20)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("media: getFile видео: %w", err)
	}
	var parsed struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.FilePath == "" {
		return nil, nil
	}
	if parsed.FileSize > MaxVideoBytes {
		return nil, nil
	}

	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	defer dcancel()
	data, err := c.downloadFileLimit(dctx, parsed.FilePath, MaxVideoBytes)
	if err != nil {
		return nil, fmt.Errorf("media: скачивание видео: %w", err)
	}
	if len(data) == 0 || len(data) > MaxVideoBytes {
		return nil, nil
	}
	return &VideoMedia{Data: data}, nil
}

// FetchVideoByFileID — см. Client.FetchVideoByFileID (фетчер персонажа).
func (m *MediaFetcher) FetchVideoByFileID(ctx context.Context, fileID string) (*VideoMedia, error) {
	return m.client.FetchVideoByFileID(ctx, fileID)
}
