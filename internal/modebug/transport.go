// HTTP-транспорт отладки (40.1/40.3): обёртка http.DefaultTransport, которая
// пишет дамп запроса/ответа и достаёт reasoning_content из ответа провайдера.
//
// Установка — modebug.InstallDefaultTransport(d) при старте приложения: ago
// создаёт http.Client без своего транспорта (internal/httpx), поэтому обёртка
// над http.DefaultTransport видит каждый модельный вызов. Запросы без Call в
// контексте (эмбеддинги, Telegram, веб) проходят насквозь без записи.
package modebug

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Transport — RoundTripper с дампом и сбором reasoning. Base nil —
// http.DefaultTransport на момент запроса. Dumper меняется только до старта
// трафика (InstallDefaultTransport под installMu): в бою установка одна.
type Transport struct {
	Base   http.RoundTripper
	Dumper *Dumper
}

// installMu защищает установку обёртки (старт процесса и тесты).
var installMu sync.Mutex

// InstallDefaultTransport вешает обёртку на http.DefaultTransport. Идемпотентно:
// повторный вызов только меняет дампер. d == nil — дамп выключен, но сбор
// reasoning_content из ответа продолжает работать (40.3).
func InstallDefaultTransport(d *Dumper) *Transport {
	installMu.Lock()
	defer installMu.Unlock()
	if tr, ok := http.DefaultTransport.(*Transport); ok {
		tr.Dumper = d
		return tr
	}
	tr := &Transport{Base: http.DefaultTransport, Dumper: d}
	http.DefaultTransport = tr
	return tr
}

// maxDumpBodyBytes — потолок сырого тела в дампе (история хода бывает большой,
// но 16 МиБ — уже авария, а не отладка).
const maxDumpBodyBytes = 16 << 20

// maxStreamDumpBytes — потолок сырых SSE-чанков в дампе (40.1: «ограниченно»).
const maxStreamDumpBytes = 2 << 20

// maxStreamDumpChunks — потолок числа чанков в дампе.
const maxStreamDumpChunks = 2000

// RoundTrip выполняет запрос и (при включённом дампе) записывает пару
// запрос/ответ. Тело ответа возвращается вызывающему без изменений.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		// Нулевое значение Transport не должно рекурсировать: если обёртка
		// сама стоит в http.DefaultTransport — берём чистый транспорт.
		if def, ok := http.DefaultTransport.(*Transport); ok && def == t {
			base = &http.Transport{}
		} else {
			base = http.DefaultTransport
		}
	}
	call := CallFromContext(req.Context())
	if call == nil {
		// Не модельный вызов хода (эмбеддинги, веб, Telegram): полный
		// pass-through, тело не читаем и не буферизуем — чужие лимиты
		// (например 16 МиБ в memory/embed.go) продолжают работать.
		return base.RoundTrip(req)
	}
	dump := t.Dumper != nil

	var reqBody []byte
	if dump && req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	seq := 0
	started := time.Now()
	if dump {
		seq = call.NextSeq()
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		if dump {
			t.Dumper.Write(t.record(call, seq, req, reqBody, started, 0, nil, err))
		}
		return resp, err
	}

	// Стрим: тело нельзя прочитать целиком до вызывающего — оборачиваем в
	// читатель, который копит чанки, достаёт reasoning и (при включённом
	// дампе) пишет файл по завершении. Обёртка нужна и без дампа: сбор
	// reasoning_content (40.3) не должен зависеть от MODEL_DEBUG_DUMP_DIR.
	// Цена: до 2 МиБ чанков в памяти на стрим даже без дампа — так reasoning
	// и склеивается (n4 ревью); ход сейчас не стримит, так что цена нулевая.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		// call здесь всегда не nil: без него был ранний pass-through выше.
		resp.Body = &streamBody{rc: resp.Body, max: maxStreamDumpBytes, finish: func(raw []byte, truncated bool) {
			content, reasoning, chunks, cut := parseSSE(raw)
			call.AddReasoning(reasoning)
			if !dump {
				return
			}
			payload := map[string]any{
				"stream":    true,
				"content":   content,
				"reasoning": reasoning,
				"chunks":    chunks,
			}
			if truncated || cut {
				payload["truncated"] = true
			}
			t.Dumper.Write(t.record(call, seq, req, reqBody, started, resp.StatusCode, payload, nil))
		}}
		return resp, nil
	}

	data, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if readErr != nil {
		if dump {
			t.Dumper.Write(t.record(call, seq, req, reqBody, started, resp.StatusCode, nil, readErr))
		}
		return nil, readErr
	}

	// reasoning_content — единственное место, где он есть в не-стрим ответе:
	// ago v0.6.0 читает только message.reasoning (40.3).
	if call != nil {
		call.AddReasoning(reasoningFromResponse(data))
	}
	if dump {
		t.Dumper.Write(t.record(call, seq, req, reqBody, started, resp.StatusCode, dumpPayload(data), nil))
	}
	return resp, nil
}

// record собирает запись дампа. Ошибка транспорта едет в Error, тело ответа —
// в Response (nil — ответа нет).
func (t *Transport) record(call *Call, seq int, req *http.Request, reqBody []byte,
	started time.Time, status int, response any, err error) Record {
	subj := call.Subject()
	rec := Record{
		Call:      seq,
		At:        started,
		URL:       redactURL(req.URL),
		Headers:   dumpHeaders(req.Header),
		Request:   dumpPayload(reqBody),
		Response:  response,
		Status:    status,
		LatencyMs: time.Since(started).Milliseconds(),
	}
	if subj.Kind == "turn" {
		rec.TurnID = parseID(subj.ID)
		if rec.TurnID == 0 {
			rec.Task = subj.ID
		}
	} else {
		rec.Task = subj.ID
	}
	if err != nil {
		// *url.Error несёт полный URL: секрет в query endpoint не должен
		// утечь в файл через поле error (M1 ревью 2026-11-06).
		msg := err.Error()
		if req != nil && req.URL != nil {
			msg = strings.ReplaceAll(msg, req.URL.String(), redactURL(req.URL))
		}
		rec.Error = msg
	}
	return rec
}

// parseID — номер хода из строки; не число — 0 (уйдёт в Task).
func parseID(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// dumpHeaders копирует заголовки запроса без Authorization: ключ API не должен
// оказаться в файле (40.1). x-opencode-session пишется — он нужен replay (40.2).
// Имена приведены к нижнему регистру: файл читает человек и replay, а не сервер.
func dumpHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, values := range h {
		if sensitiveHeader(k) {
			continue
		}
		out[strings.ToLower(k)] = strings.Join(values, ", ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sensitiveHeader — заголовок, который может нести credential: не только
// Authorization (смена провайдера не должна тихо писать ключ в файл).
func sensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	for _, marker := range []string{
		"authorization", "api-key", "api_key", "apikey", "x-api-key",
		"auth-token", "token", "secret", "cookie", "proxy-authorization",
	} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

// sensitiveQueryParam — имя параметра URL, значение которого может быть
// секретом: endpoint в конфиге иногда содержит ключ в query.
func sensitiveQueryParam(name string) bool {
	name = strings.ToLower(name)
	for _, marker := range []string{"key", "token", "secret", "password", "passwd", "signature", "sig", "auth"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// redactURL — адрес для дампа: секретные значения query заменяются на
// REDACTED (заголовок Authorization и так не пишется). Прочие параметры
// сохраняются — replay должен попасть на тот же адрес (40.2).
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	if cp.User != nil {
		cp.User = url.User("REDACTED") // userinfo может содержать ключ
	}
	if cp.RawQuery != "" {
		q := cp.Query()
		for name := range q {
			if sensitiveQueryParam(name) {
				q.Set(name, "REDACTED")
			}
		}
		cp.RawQuery = q.Encode()
	}
	return cp.String()
}

// dumpPayload — тело для дампа: JSON/строка, но не больше maxDumpBodyBytes
// (большие тела — с пометкой truncated и началом; полный запрос в таком
// дампе replay не восстановит, зато диск и память не растут без предела).
func dumpPayload(data []byte) any {
	if len(data) > maxDumpBodyBytes {
		return map[string]any{
			"truncated": true,
			"bytes":     len(data),
			"prefix":    string(data[:maxDumpBodyBytes]),
		}
	}
	return jsonOrString(data)
}

// jsonOrString — тело как JSON, если это JSON, иначе строкой (ошибки HTML и
// прочий мусор не должны ломать сериализацию дампа).
func jsonOrString(data []byte) any {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if json.Valid(trimmed) {
		return json.RawMessage(trimmed)
	}
	return string(data)
}

// reasoningFromResponse достаёт message.reasoning_content из не-стрим ответа
// Chat Completions. Пусто — провайдер reasoning не отдавал.
func reasoningFromResponse(data []byte) string {
	var raw struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
				// Некоторые диалекты зовут поле reasoning (ago читает его).
				Reasoning string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range raw.Choices {
		if c.Message.ReasoningContent != "" {
			b.WriteString(c.Message.ReasoningContent)
			continue
		}
		b.WriteString(c.Message.Reasoning)
	}
	return b.String()
}

// streamBody копит прочитанные SSE-байты (до max) и один раз вызывает finish
// при EOF/Close. Оборванное чтение (Close без EOF) помечается truncated:
// иначе дамп выглядел бы полным. Ошибки чтения не глотаются.
type streamBody struct {
	rc     io.ReadCloser
	mu     sync.Mutex
	buf    bytes.Buffer
	max    int
	cut    bool
	eof    bool
	once   sync.Once
	finish func(raw []byte, truncated bool)
}

func (s *streamBody) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.mu.Lock()
		if remain := s.max - s.buf.Len(); remain > 0 {
			if n <= remain {
				_, _ = s.buf.Write(p[:n])
			} else {
				_, _ = s.buf.Write(p[:remain])
				s.cut = true
			}
		} else {
			s.cut = true
		}
		s.mu.Unlock()
	}
	if err == io.EOF {
		s.mu.Lock()
		s.eof = true
		s.mu.Unlock()
		s.complete()
	}
	return n, err
}

func (s *streamBody) Close() error {
	s.complete()
	return s.rc.Close()
}

func (s *streamBody) complete() {
	s.once.Do(func() {
		s.mu.Lock()
		raw := append([]byte(nil), s.buf.Bytes()...)
		truncated := s.cut || !s.eof
		s.mu.Unlock()
		if s.finish != nil {
			s.finish(raw, truncated)
		}
	})
}

// parseSSE разбирает сырой SSE-поток: склеивает content и reasoning_content,
// возвращает ограниченный список сырых data-чанков (40.1).
func parseSSE(raw []byte) (content, reasoning string, chunks []string, truncated bool) {
	var c, r strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if len(chunks) < maxStreamDumpChunks {
			chunks = append(chunks, payload)
		} else {
			truncated = true
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			c.WriteString(ch.Delta.Content)
			if ch.Delta.ReasoningContent != "" {
				r.WriteString(ch.Delta.ReasoningContent)
			} else {
				r.WriteString(ch.Delta.Reasoning)
			}
		}
	}
	return c.String(), r.String(), chunks, truncated
}
