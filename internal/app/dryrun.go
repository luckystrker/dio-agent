// Dry-run заглушка Bot API (TELEGRAM_DRY_RUN=true, без токена): приём пуст,
// отправки пишутся в лог. Закрывает пробел wiring 9.5 («DryRun-клиент TS не
// перенесён»): локальный прогон без токена видит, что персонаж отправлял бы.
package app

import (
	"context"
	"log"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// dryRunAPI — telegram.API без сети: getUpdates пуст, отправки — в лог.
type dryRunAPI struct {
	logger *log.Logger
}

var _ telegram.API = (*dryRunAPI)(nil)

func (d *dryRunAPI) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error) {
	return nil, nil
}

func (d *dryRunAPI) GetMe(ctx context.Context) (telegram.BotIdentity, error) {
	return telegram.BotIdentity{}, nil
}

func (d *dryRunAPI) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	d.logger.Printf("dry-run: sendMessage chat=%d reply_to=%d: %s", chatID, replyTo, text)
	return 0, nil
}

func (d *dryRunAPI) ForwardMessage(ctx context.Context, chatID int64, fromChatID int64, messageID int64) (int64, error) {
	d.logger.Printf("dry-run: forwardMessage chat=%d from=%d message=%d", chatID, fromChatID, messageID)
	return 0, nil
}

func (d *dryRunAPI) SendSticker(ctx context.Context, chatID int64, fileID string) (int64, error) {
	d.logger.Printf("dry-run: sendSticker chat=%d: %s", chatID, fileID)
	return 0, nil
}

func (d *dryRunAPI) SendPhoto(ctx context.Context, chatID int64, filename, contentType string, data []byte) (int64, error) {
	d.logger.Printf("dry-run: sendPhoto chat=%d: %s (%s, %d байт)", chatID, filename, contentType, len(data))
	return 0, nil
}

func (d *dryRunAPI) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return nil
}

func (d *dryRunAPI) SetMessageReaction(ctx context.Context, chatID int64, messageID int64, emoji string) error {
	d.logger.Printf("dry-run: setMessageReaction chat=%d message=%d: %s", chatID, messageID, emoji)
	return nil
}
