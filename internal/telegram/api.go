// Тонкий клиент Telegram Bot API на net/http (без сторонних библиотек).
// Поведение — src/telegram/api.ts: внедряемый транспорт для тестов
// (httptest) и локальных прогонов.
//
// Таймаут — на каждый вызов (контекстом), 429 отдаёт retry_after в типе
// ошибки. Сам клиент ничего не повторяет: решение о повторе принимает
// вызывающий по политике доставки (§13.2) или политике polling (§6.1).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Update — сырой update из getUpdates: номер для дедупликации (§6.1) и
// исходный JSON для social_events.raw_update.
type Update struct {
	UpdateID int64
	Raw      json.RawMessage
}

// APIError — подтверждённая ошибка Bot API: ответ получен, отправки не было.
type APIError struct {
	Method     string
	HTTPStatus int
	// RetryAfter — пауза из parameters.retry_after у 429 (§13.2); 0 — нет.
	RetryAfter  time.Duration
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d — %s", e.Method, e.HTTPStatus, e.Description)
}

// Client — Bot API клиент. Реализует интерфейс API; метод SendChatAction
// совпадает по сигнатуре с character.ChatActionSender (typing-пульс §13.3).
type Client struct {
	base string
	// fileBase — файловый хост Bot API: <apiBase>/file/bot<token> (формат
	// скачивания по file_path из getFile отличается от методов).
	fileBase string
	// token — только для затирания в текстах ошибок (журнал не должен
	// получать токен открытым текстом, см. redact).
	token string
	http  *http.Client
	// sendTimeout — sendMessage/setMessageReaction (сверен с TS delivery 10s).
	sendTimeout time.Duration
	// actionTimeout — sendChatAction «печатает…» (5s, TS).
	actionTimeout time.Duration
	// updatesGrace — запас сверх long-poll таймаута getUpdates.
	updatesGrace time.Duration
}

// NewClient строит клиент: base = apiBase/bot<token>. httpClient nil —
// дефолтный транспорт с жёстким tcp4 (инцидент 2026-10-31: IPv6-флап
// провайдера — TCP устанавливался, данные висели в send-Q; Bot API доступен
// по IPv4 всегда); тесты подставляют свой, чтобы крутить таймауты и транспорт.
func NewClient(botToken, apiBase string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Transport: defaultTransport()}
	}
	return &Client{
		base:          strings.TrimRight(apiBase, "/") + "/bot" + botToken,
		fileBase:      strings.TrimRight(apiBase, "/") + "/file/bot" + botToken,
		token:         botToken,
		http:          httpClient,
		sendTimeout:   10 * time.Second,
		actionTimeout: 5 * time.Second,
		updatesGrace:  5 * time.Second,
	}
}

// defaultTransport — транспорт, всегда набирающий IPv4. Меняет запрошенную
// сеть «tcp»/«tcp6» на «tcp4» независимо от адреса.
func defaultTransport() *http.Transport {
	dialer := &net.Dialer{}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
}

// redactedError — ошибка с затёртым токеном в тексте. Unwrap сохраняет
// errors.Is/As: классификация доставки §13.2 различает таймаут и обрыв.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// requestError — ошибка транспорта без ответа Bot API с признаком, было ли
// тело запроса записано в соединение. До записи запрос точно не отправлен —
// это временный отказ (retry_wait), после записи исход неизвестен (unknown).
type requestError struct {
	err          error
	wroteRequest bool
}

func (e *requestError) Error() string { return e.err.Error() }
func (e *requestError) Unwrap() error { return e.err }

// redact прячет токен в тексте ошибки (URL запроса вида /bot<token>/… попадает
// в url.Error, а он — в журнал; инцидент 2026-10-31: токен лежал в journald
// открытым текстом).
func (c *Client) redact(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return &redactedError{msg: strings.ReplaceAll(err.Error(), c.token, "[токен]"), err: err}
}

// BotIdentity — кто такой этот бот (getMe): нужен, чтобы в группе reply на
// чужое сообщение и @чужой_ник не считались прямым обращением (M8).
type BotIdentity struct {
	ID       int64
	Username string
}

// API — интерфейс Telegram-слоя для poller и delivery (подменяется в тестах).
type API interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error)
	GetMe(ctx context.Context) (BotIdentity, error)
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
	SendSticker(ctx context.Context, chatID int64, fileID string) (int64, error)
	SendPhoto(ctx context.Context, chatID int64, filename, contentType string, data []byte) (int64, error)
	SendChatAction(ctx context.Context, chatID int64, action string) error
	SetMessageReaction(ctx context.Context, chatID int64, messageID int64, emoji string) error
}

var _ API = (*Client)(nil)

// GetUpdates — long polling (§6.1): ждёт обновления с номером >= offset до
// timeoutSec. Таймаут вызова = timeoutSec + запас на ответ.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second+c.updatesGrace)
	defer cancel()

	raw, err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message", "edited_message"},
	}, 0) // тело читается целиком: пачка-бёрст может занимать мегабайты, обрезанный JSON = вечный повтор пачки (§6.1)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("getUpdates: result не массив: %w", err)
		}
	}
	updates := make([]Update, 0, len(items))
	for _, item := range items {
		var id struct {
			UpdateID int64 `json:"update_id"`
		}
		if err := json.Unmarshal(item, &id); err != nil {
			return nil, fmt.Errorf("getUpdates: update без update_id: %w", err)
		}
		updates = append(updates, Update{UpdateID: id.UpdateID, Raw: item})
	}
	return updates, nil
}

// GetMe — идентичность бота (id + username) для распознавания обращений в
// группе (M8).
func (c *Client) GetMe(ctx context.Context) (BotIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, c.actionTimeout)
	defer cancel()
	raw, err := c.call(ctx, "getMe", map[string]any{}, maxSmallBody)
	if err != nil {
		return BotIdentity{}, err
	}
	var result struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.ID == 0 {
		return BotIdentity{}, &APIError{Method: "getMe", HTTPStatus: 200, Description: "ответ без id бота"}
	}
	return BotIdentity{ID: result.ID, Username: result.Username}, nil
}

// SendMessage отправляет текст и возвращает Telegram message_id.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()

	raw, err := c.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, maxSmallBody)
	if err != nil {
		return 0, err
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.MessageID == 0 {
		return 0, &APIError{Method: "sendMessage", HTTPStatus: 200,
			Description: "ответ без message_id"}
	}
	return result.MessageID, nil
}

// SendSticker отправляет стикер из коллекции персонажа (этап 25.4) и
// возвращает Telegram message_id: file_id выбирает runtime, не модель.
func (c *Client) SendSticker(ctx context.Context, chatID int64, fileID string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()

	raw, err := c.call(ctx, "sendSticker", map[string]any{"chat_id": chatID, "sticker": fileID}, maxSmallBody)
	if err != nil {
		return 0, err
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.MessageID == 0 {
		return 0, &APIError{Method: "sendSticker", HTTPStatus: 200,
			Description: "ответ без message_id"}
	}
	return result.MessageID, nil
}

// SendPhoto отправляет картинку файлом (multipart, sendPhoto): модель называет
// URL, байты качает и отдаёт runtime — Bot API не должен ходить по адресу
// модели сам (этап 36.2).
func (c *Client) SendPhoto(ctx context.Context, chatID int64, filename, contentType string, data []byte) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return 0, fmt.Errorf("sendPhoto: chat_id не записан: %w", err)
	}
	header := make(textproto.MIMEHeader, 2)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="photo"; filename="%s"`, filename))
	header.Set("Content-Type", contentType)
	part, err := mw.CreatePart(header)
	if err != nil {
		return 0, fmt.Errorf("sendPhoto: часть multipart не создана: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return 0, fmt.Errorf("sendPhoto: файл не записан: %w", err)
	}
	if err := mw.Close(); err != nil {
		return 0, fmt.Errorf("sendPhoto: multipart не закрыт: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/sendPhoto", &buf)
	if err != nil {
		return 0, fmt.Errorf("sendPhoto: запрос не собран: %w", err)
	}
	req.Header.Set("content-type", mw.FormDataContentType())

	raw, err := c.do(ctx, "sendPhoto", req, maxSmallBody)
	if err != nil {
		return 0, err
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.MessageID == 0 {
		return 0, &APIError{Method: "sendPhoto", HTTPStatus: 200,
			Description: "ответ без message_id"}
	}
	return result.MessageID, nil
}

// photoFilename — имя файла для sendPhoto по типу: Telegram определяет тип по
// содержимому, но расширение помогает клиенту собеседника.
func photoFilename(contentType string) string {
	switch contentType {
	case "image/png":
		return "photo.png"
	case "image/webp":
		return "photo.webp"
	case "image/gif":
		return "photo.gif"
	default:
		return "photo.jpg"
	}
}

// SendChatAction — «печатает…» и подобные (короткий fire-and-forget вызов
// §13.3; решение глотать ошибки принимает вызывающий — пульс).
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	ctx, cancel := context.WithTimeout(ctx, c.actionTimeout)
	defer cancel()
	_, err := c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, maxSmallBody)
	return err
}

// SetMessageReaction ставит реакцию на сообщение (§11.2 decision=react).
// Контракт Bot API: параметр reaction — список ReactionType; в JSON-теле он
// идёт настоящим массивом. Параметр «emoji» Telegram игнорирует как
// неизвестный и отвечает ok, не поставив ничего (находка приёмки Р).
func (c *Client) SetMessageReaction(ctx context.Context, chatID int64, messageID int64, emoji string) error {
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)
	defer cancel()
	_, err := c.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": chatID, "message_id": messageID,
		"reaction": []map[string]string{{"type": "emoji", "emoji": emoji}},
	}, maxSmallBody)
	return err
}

// apiEnvelope — стандартная обёртка ответа Bot API.
type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// maxSmallBody — защитный лимит тела ответа мелких методов (sendMessage,
// setMessageReaction, sendChatAction): нормальный результат там укладывается
// с запасом. getUpdates этот лимит не использует (maxBody 0): пачка-бёрст
// из сотни сообщений с длинными текстами занимает мегабайты, а обрезанный
// JSON не парсится — polling вечно повторял бы ту же пачку (§6.1).
const maxSmallBody = 1 << 20

// call выполняет один POST-запрос метода и возвращает result успешного ответа;
// maxBody 0 — читать тело целиком.
func (c *Client) call(ctx context.Context, method string, payload any, maxBody int64) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%s: payload не сериализуется: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: запрос не собран: %w", method, err)
	}
	req.Header.Set("content-type", "application/json")
	return c.do(ctx, method, req, maxBody)
}

// do выполняет готовый запрос метода и возвращает result успешного ответа;
// maxBody 0 — читать тело целиком. Общий путь JSON-методов и sendPhoto
// (multipart): классификация «отправка не начиналась / исход неизвестен»
// должна быть одинаковой у всех вызовов (§13.2).
func (c *Client) do(ctx context.Context, method string, req *http.Request, maxBody int64) (json.RawMessage, error) {
	// httptrace фиксирует, ушёл ли запрос в сеть: dial/DNS/отказ соединения
	// до записи тела — это заведомо несостоявшаяся отправка (C2), её можно
	// повторить; обрыв после записи — unknown.
	var wroteRequest atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := c.http.Do(req)
	if err != nil {
		// Ответа нет: исход запроса может быть неизвестен — ошибка уходит как
		// есть (с признаком записи), политика доставки классифицирует её.
		return nil, fmt.Errorf("%s: %w", method, c.redact(&requestError{
			err:          err,
			wroteRequest: wroteRequest.Load(),
		}))
	}
	defer resp.Body.Close() //nolint:errcheck // чтение тела ниже, закрытие не влияет на результат

	var raw []byte
	if maxBody > 0 {
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	} else {
		raw, err = io.ReadAll(resp.Body) // целиком: см. getUpdates
	}
	if err != nil {
		return nil, &APIError{Method: method, HTTPStatus: resp.StatusCode,
			Description: "тело ответа не читается"}
	}

	var envelope apiEnvelope
	_ = json.Unmarshal(raw, &envelope) // не-JSON тело даёт нулевую обёртку — ошибка ниже
	if resp.StatusCode >= 400 || !envelope.OK {
		apiErr := &APIError{
			Method:      method,
			HTTPStatus:  resp.StatusCode,
			Description: envelope.Description,
		}
		if apiErr.Description == "" {
			apiErr.Description = resp.Status
		}
		if envelope.Parameters != nil && envelope.Parameters.RetryAfter > 0 {
			apiErr.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		}
		return nil, apiErr
	}
	return envelope.Result, nil
}
