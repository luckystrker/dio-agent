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
	"net"
	"net/http"
	"strings"
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

// redact прячет токен в тексте ошибки (URL запроса вида /bot<token>/… попадает
// в url.Error, а он — в журнал; инцидент 2026-10-31: токен лежал в journald
// открытым текстом).
func (c *Client) redact(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return &redactedError{msg: strings.ReplaceAll(err.Error(), c.token, "[токен]"), err: err}
}

// API — интерфейс Telegram-слоя для poller и delivery (подменяется в тестах).
type API interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
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

	resp, err := c.http.Do(req)
	if err != nil {
		// Ответа нет: исход запроса неизвестен — ошибка уходит как есть,
		// политика доставки классифицирует её как unknown (§13.2).
		return nil, fmt.Errorf("%s: %w", method, c.redact(err))
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
