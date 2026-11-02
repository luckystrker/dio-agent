// Фото из Telegram (§6.1): file_id из raw_update, скачивание через Bot API.
// Картинка живёт только в памяти хода: на диск не пишем, в журнал не кладём
// (в raw_update она и так есть). Портировано из TS-линии da9559d, 2026-10-31.
package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MaxPhotoBytes — потолок скачивания: больше в контекст не тянем
// (Telegram-фото обычно 50–300 КБ).
const MaxPhotoBytes = 6 << 20 // 6 MiB

// MaxImagesPerTurn — больше трёх фото в один ход не тянем: контекст и деньги.
const MaxImagesPerTurn = 3

// PhotoFileIDFromUpdate — file_id крупнейшего PhotoSize из raw_update
// (по площади; Telegram отдаёт размеры по возрастанию). Пусто — фото нет.
// Фото может быть в самом сообщении или в цитируемом reply_to_message:
// normalize пишет «[в ответ на X: (фото)]», и без этого картинка не доходила
// до модели (bug 2026-11-02). Собственное фото сообщения приоритетнее.
func PhotoFileIDFromUpdate(raw []byte) string {
	var shape struct {
		Message           *messagePhotoJSON `json:"message"`
		EditedMessage     *messagePhotoJSON `json:"edited_message"`
		ChannelPost       *messagePhotoJSON `json:"channel_post"`
		EditedChannelPost *messagePhotoJSON `json:"edited_channel_post"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return ""
	}
	var photos []photoSizeJSON
	for _, m := range []*messagePhotoJSON{shape.Message, shape.EditedMessage, shape.ChannelPost, shape.EditedChannelPost} {
		if m == nil {
			continue
		}
		photos = m.photoSizes()
		if len(photos) > 0 {
			break
		}
	}
	best := ""
	bestArea := -1
	for _, p := range photos {
		if p.FileID == "" {
			continue
		}
		if area := p.Width * p.Height; area > bestArea {
			bestArea = area
			best = p.FileID
		}
	}
	return best
}

// messagePhotoJSON — минимальная форма сообщения для поиска фото: своё .photo
// и фото цитируемого reply_to_message.
type messagePhotoJSON struct {
	Photo          []photoSizeJSON   `json:"photo"`
	ReplyToMessage *messagePhotoJSON `json:"reply_to_message"`
}

// photoSizes — фото сообщения; собственное приоритетнее цитируемого.
func (m *messagePhotoJSON) photoSizes() []photoSizeJSON {
	if len(m.Photo) > 0 {
		return m.Photo
	}
	if m.ReplyToMessage != nil {
		return m.ReplyToMessage.Photo
	}
	return nil
}

type photoSizeJSON struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// PhotoMedia — скачанное фото: base64 без data:-префикса.
type PhotoMedia struct {
	Mime   string
	Base64 string
}

// Data — сырые байты (base64 декодируется лениво; строка наша — ошибки нет).
func (m *PhotoMedia) Data() []byte {
	b, _ := base64.StdEncoding.DecodeString(m.Base64)
	return b
}

// FetchPhotoByFileID качает фото по file_id: getFile → file_path → байты.
// Ошибка/слишком большой файл — (nil, nil): ход продолжается по тексту.
func (c *Client) FetchPhotoByFileID(ctx context.Context, fileID string) (*PhotoMedia, error) {
	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	raw, err := c.call(gctx, "getFile", map[string]string{"file_id": fileID}, 1<<20)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("media: getFile: %w", err)
	}
	// c.call возвращает уже result-объект (обёртка ok/result снята).
	var parsed struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.FilePath == "" {
		return nil, nil
	}
	filePath := parsed.FilePath

	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	defer dcancel()
	data, err := c.downloadFile(dctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("media: download: %w", err)
	}
	if len(data) == 0 || len(data) > MaxPhotoBytes {
		return nil, nil
	}
	mime := "image/jpeg"
	if len(filePath) >= 4 && filePath[len(filePath)-4:] == ".png" {
		mime = "image/png"
	}
	return &PhotoMedia{Mime: mime, Base64: base64.StdEncoding.EncodeToString(data)}, nil
}

// downloadFile качает файл по пути из getFile (файловый хост Bot API).
func (c *Client) downloadFile(ctx context.Context, filePath string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/"+url.PathEscape(filePath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.redact(err)
	}
	defer resp.Body.Close() //nolint:errcheck // тело уже прочитано
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media: статус %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, MaxPhotoBytes+1))
}

// MediaFetcher — character.PhotoFetcher поверх Bot API (свой http-клиент).
type MediaFetcher struct {
	client *Client
}

// NewMediaFetcher строит фетчер по токену и базе Bot API.
func NewMediaFetcher(token, apiBase string) *MediaFetcher {
	return &MediaFetcher{client: NewClient(token, apiBase, nil)}
}

// FetchPhotoByFileID — см. Client.FetchPhotoByFileID.
func (m *MediaFetcher) FetchPhotoByFileID(ctx context.Context, fileID string) (*PhotoMedia, error) {
	return m.client.FetchPhotoByFileID(ctx, fileID)
}
