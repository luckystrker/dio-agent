// Replay дампа (40.2): повторить запрос к тому же провайдеру и посмотреть
// ответ. Ничего не пишет в БД и не шлёт в Telegram: только HTTP к модели.
package modebug

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Dump — прочитанный дамп модельного вызова. Request — тело как ушло; Headers
// без Authorization (ключа в файле нет).
type Dump struct {
	TurnID  int64             `json:"turn_id"`
	Task    string            `json:"task"`
	Call    int               `json:"call"`
	At      time.Time         `json:"at"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Request json.RawMessage   `json:"request"`
	Status  int               `json:"status"`
}

// LoadDump читает файл дампа.
func LoadDump(path string) (*Dump, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("дамп %q не читается: %w", path, err)
	}
	var d Dump
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("дамп %q не JSON (40.1 пишет файл на вызов): %w", path, err)
	}
	request := bytes.TrimSpace(d.Request)
	if len(request) == 0 || request[0] != '{' {
		// request:null проходит проверку на непустоту, но это не запрос:
		// абляция temperature упала бы присваиванием в nil-map.
		return nil, fmt.Errorf("дамп %q: пустой request — нужен JSON-объект", path)
	}
	var probe struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(request, &probe); err == nil && probe.Truncated {
		// Дамп >16 МиБ пишется заглушкой {truncated,prefix}: replay такого
		// запроса бессмыслен (m7 ревью).
		return nil, fmt.Errorf("дамп %q: request усечён при записи (>%d МиБ) — replay невозможен", path, maxDumpBodyBytes>>20)
	}
	return &d, nil
}

// Replayer отправляет запрос провайдеру. Endpoint — база из MODEL_ENDPOINT
// (путь /chat/completions добавляется, если его нет); APIKey — ключ из .env,
// Client — тестовый или nil (обычный http.Client с таймаутом).
type Replayer struct {
	Endpoint string
	APIKey   string
	Client   *http.Client
	Timeout  time.Duration
}

// Reply — ответ провайдера.
type Reply struct {
	// URL — куда реально ушёл запрос (нужен логу replay).
	URL string
	// Status — HTTP-код.
	Status int
	// Body — сырое тело ответа.
	Body []byte
	// Latency — сколько занял запрос.
	Latency time.Duration
}

// Send выполняет один replay-запрос. url пуст — Endpoint из конфигурации;
// непустой (из дампа) — тот же адрес, что у живого вызова.
func (r *Replayer) Send(ctx context.Context, url string, request map[string]any, headers map[string]string) (*Reply, error) {
	target := strings.TrimSpace(url)
	if target == "" {
		target = r.Endpoint
	}
	target = ensureChatPath(target)
	if target == "" {
		return nil, fmt.Errorf("replay: пустой MODEL_ENDPOINT и нет url в дампе")
	}
	// Ключ из .env уходит только на хост MODEL_ENDPOINT: url берётся из файла
	// дампа, а файл можно скачать/подменить — credential не должен уехать
	// чужому хосту.
	if err := r.sameProviderHost(target); err != nil {
		return nil, err
	}
	// replay всегда не-стрим: SSE-ответ нельзя разобрать как Chat Completions,
	// иначе вердикт был бы ложным «пустой content».
	if stream, ok := request["stream"].(bool); ok && stream {
		request["stream"] = false
		delete(request, "stream_options")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("replay: запрос не сериализуется: %w", err)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: r.Timeout}
	}
	if r.Timeout > 0 && r.Client != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("replay: запрос не собран: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
	for k, v := range headers {
		switch {
		case sensitiveHeader(k), strings.EqualFold(k, "content-type"), strings.EqualFold(k, "accept"):
			// Ключ и тип тела — из конфигурации, не из файла.
		default:
			req.Header.Set(k, v)
		}
	}

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error печатает полный URL (иногда с замаскированным паролем):
		// секреты query/userinfo не должны попасть в терминал/лог replay.
		return nil, fmt.Errorf("replay: провайдер недоступен: %s", redactErrorMessage(err.Error(), req.URL))
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	reply := &Reply{URL: target, Status: resp.StatusCode, Body: body, Latency: time.Since(started)}
	if readErr != nil {
		return reply, fmt.Errorf("replay: ответ не прочитан: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return reply, fmt.Errorf("replay: провайдер ответил %d: %.500s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return reply, nil
}

// sameProviderHost — схема и host url (дампа или endpoint) должны совпадать с
// MODEL_ENDPOINT: ключ не отправляется чужому хосту. Кривой endpoint — отказ,
// а не «поверим файлу»: направление ошибки должно быть безопасным (m6 ревью).
func (r *Replayer) sameProviderHost(target string) error {
	if strings.TrimSpace(r.Endpoint) == "" {
		return nil
	}
	ep, err := url.Parse(ensureChatPath(r.Endpoint))
	if err != nil || ep.Host == "" {
		return fmt.Errorf("replay: MODEL_ENDPOINT %q не разобран — ключ не отправляю; проверь .env", redactTarget(r.Endpoint))
	}
	tp, err := url.Parse(target)
	if err != nil || tp.Host == "" {
		return fmt.Errorf("replay: url %q не разобран — ключ не отправляю", redactTarget(target))
	}
	if !strings.EqualFold(ep.Scheme, tp.Scheme) || !strings.EqualFold(ep.Host, tp.Host) {
		return fmt.Errorf("replay: url из дампа (%s://%s) не совпадает со схемой/хостом MODEL_ENDPOINT (%s://%s) — ключ не отправляется чужому хосту; обнови MODEL_ENDPOINT или используй дамп того же провайдера",
			tp.Scheme, tp.Host, ep.Scheme, ep.Host)
	}
	return nil
}

// redactTarget — адрес для сообщений об ошибках: без секретов query/userinfo.
func redactTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "<неразобранный url>"
	}
	return redactURL(u)
}

// ensureChatPath добавляет /chat/completions, если адрес — только база.
// Путь меняется у разобранного URL, а не конкатенацией строки: иначе endpoint
// с query (?api_key=…) получил бы путь после query (находка критика).
func ensureChatPath(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		base = strings.TrimSuffix(base, "/")
		if strings.HasSuffix(base, "/chat/completions") {
			return base
		}
		return base + "/chat/completions"
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(u.Path, "/chat/completions") {
		return u.String()
	}
	u.Path += "/chat/completions"
	return u.String()
}

// ChatReply — разобранный не-стрим ответ Chat Completions: то, что нужно
// replay напечатать (content и reasoning_content).
type ChatReply struct {
	Content      string
	Reasoning    string
	FinishReason string
	Usage        map[string]any
}

// ParseChatResponse разбирает ответ провайдера. Ошибка разбора — пустой ответ
// (replay печатает сырое тело отдельно).
func ParseChatResponse(body []byte) ChatReply {
	var raw struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Choices) == 0 {
		return ChatReply{}
	}
	c := raw.Choices[0]
	out := ChatReply{
		Content:      c.Message.Content,
		Reasoning:    c.Message.ReasoningContent,
		FinishReason: c.FinishReason,
		Usage:        raw.Usage,
	}
	if out.Reasoning == "" {
		out.Reasoning = c.Message.Reasoning
	}
	return out
}
