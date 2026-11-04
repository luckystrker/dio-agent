// Аватарки (34.2): getUserProfilePhotos (limit 1) и скачивание через
// getFile. Отдельный фетчер, чтобы API интерфейс poller/delivery не раздувался
// методами, нужными только фоновой задаче.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
)

// ProfilePhoto — аватарка пользователя: file_id/file_unique_id крупнейшего
// размера первой фотографии. Пустой FileID — аватарки нет или она скрыта
// настройками приватности.
type ProfilePhoto struct {
	FileID       string
	FileUniqueID string
	Width        int
	Height       int
}

// GetUserProfilePhotos — последняя аватарка пользователя (limit 1). Ошибка
// Bot API возвращается как есть: фоновый checker сам решает про повтор.
func (c *Client) GetUserProfilePhotos(ctx context.Context, userID int64) (ProfilePhoto, error) {
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()
	raw, err := c.call(ctx, "getUserProfilePhotos", map[string]any{
		"user_id": userID,
		"limit":   1,
	}, maxSmallBody)
	if err != nil {
		return ProfilePhoto{}, err
	}
	var result struct {
		TotalCount int `json:"total_count"`
		Photos     [][]struct {
			FileID       string `json:"file_id"`
			FileUniqueID string `json:"file_unique_id"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
		} `json:"photos"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ProfilePhoto{}, &APIError{Method: "getUserProfilePhotos", HTTPStatus: 200,
			Description: "ответ не разобран: " + err.Error()}
	}
	if result.TotalCount == 0 || len(result.Photos) == 0 || len(result.Photos[0]) == 0 {
		return ProfilePhoto{}, nil
	}
	var best ProfilePhoto
	bestArea := -1
	for _, p := range result.Photos[0] {
		if p.FileID == "" {
			continue
		}
		if area := p.Width * p.Height; area > bestArea {
			bestArea = area
			best = ProfilePhoto{FileID: p.FileID, FileUniqueID: p.FileUniqueID,
				Width: p.Width, Height: p.Height}
		}
	}
	return best, nil
}

// AvatarFetcher — источник аватарок для фоновой задачи (internal/background).
type AvatarFetcher struct {
	client *Client
}

// NewAvatarFetcher строит фетчер по токену и базе Bot API.
func NewAvatarFetcher(token, apiBase string) *AvatarFetcher {
	return &AvatarFetcher{client: NewClient(token, apiBase, nil)}
}

// ProfilePhoto — file_id и file_unique_id текущей аватарки; пусто — нет.
func (f *AvatarFetcher) ProfilePhoto(ctx context.Context, userID int64) (fileID, fileUniqueID string, err error) {
	photo, err := f.client.GetUserProfilePhotos(ctx, userID)
	if err != nil {
		return "", "", err
	}
	return photo.FileID, photo.FileUniqueID, nil
}

// FetchAvatar — байты и MIME аватарки по file_id (getFile + скачивание).
func (f *AvatarFetcher) FetchAvatar(ctx context.Context, fileID string) (mime string, data []byte, err error) {
	media, err := f.client.FetchPhotoByFileID(ctx, fileID)
	if err != nil {
		return "", nil, err
	}
	if media == nil {
		return "", nil, fmt.Errorf("аватарки: файл не скачан или больше %d байт", MaxPhotoBytes)
	}
	return media.Mime, media.Data(), nil
}
