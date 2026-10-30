// Package telegram — Telegram-слой (§4, §6, §13): приём событий, буферизация
// и доставка.
//
// Модуль 9.5:
//   - тонкий клиент Bot API на net/http (api.go): getUpdates long polling,
//     sendMessage, sendChatAction, setMessageReaction; таймаут на каждый
//     вызов, 429 отдаёт retry_after в типе ошибки; транспорт внедряется
//     (httptest-тесты); клиент ничего не повторяет сам — политику решает
//     вызывающий (§13.2);
//   - нормализация и prefilter (normalize.go, prefilter.go §6.3): прямой
//     запрос не отбрасывается, шум группы — journal_only;
//   - ingest §6.1 (ingest.go): короткая транзакция на update — событие с
//     UNIQUE(telegram_update_id) и следующий poll offset атомарно; дубль
//     пачки = «уже сохранено», unsupported updates пишутся journal_only,
//     чтобы offset не застревал; при ошибке БД offset не двигается;
//   - буферы чатов §6.2 (buffers.go): окна личка/группа/reply-mention-command,
//     закрытие по ближайшему из idle/max сроков, по 20 событиям или лимиту
//     текста; закрытие транзакционно создаёт turn и привязывает события;
//     recovery pending-событий после падения (§12.2);
//   - outbox и delivery loop §13.1–13.3 (outbox.go, delivery.go): статусы
//     pending/sending/sent/retry_wait/unknown/cancelled/failed, порядок
//     внутри чата, unknown-политика (таймаут/обрыв после возможной отправки
//     → unknown без автоповтора, зависимые отменяются), устаревание перед
//     отправкой (новые события чата отменяют оставшиеся bubbles) и пауза
//     между bubbles;
//   - polling (poller.go): getUpdates → ingest → буферы.
//
// Строки delivery_outbox создаёт применение хода (character.ApplyTurn,
// событие delivery_queued §12.1 — в event_streams); этот модуль их отправляет.
// Клиент реализует character.ChatActionSender по структурной типизации
// (typing-пульс §13.3), без импорта character.
package telegram
