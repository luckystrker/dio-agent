// Пакет modebug — отладка модельных вызовов на уровне HTTP-транспорта
// (этап 40): дамп ровно того, что ушло провайдеру и пришло от него (40.1), и
// добыча reasoning_content, которую ago в не-стрим ответе не разбирает (40.3).
//
// Почему транспорт, а не middleware: провайдер ago строит http.Client сам
// (internal/httpx: HTTP == nil → клиент с http.DefaultTransport), и другого
// честного места «что реально ушло на провод» нет — middleware видит события,
// а не тело запроса. Обёртка http.DefaultTransport видит и заголовки, и тело,
// и точный ответ, и не зависит от версии ago.
package modebug

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
)

// Subject — кто вызвал модель: разговорный ход или фоновая задача. Имя файла
// дампа — turn-<id>-call-<n>.json / bg-<task>-call-<n>.json (40.1).
type Subject struct {
	// Kind — "turn" или "bg".
	Kind string
	// ID — номер хода или имя фоновой задачи (например "intention:42").
	ID string
}

// Call — один логический вызов модели (ход или фоновая задача) в контексте
// HTTP-запроса: счётчик вызовов для имени дампа и накопитель reasoning_content
// из транспорта. Указатель кладётся в context (без глобалов), поэтому
// параллельные ходы не смешивают счётчики и reasoning.
type Call struct {
	subject Subject

	seq atomic.Int64

	mu        sync.Mutex
	reasoning []string
}

// maxCallReasoningBytes — потолок накопленного reasoning в Call: провайдер не
// должен раздувать память процесса (журналу и так хватает 4000 рун).
const maxCallReasoningBytes = 64 * 1024

// NewCall создаёт счётчик и накопитель reasoning для одного хода/задачи.
func NewCall(kind, id string) *Call {
	return &Call{subject: Subject{Kind: kind, ID: id}}
}

// Subject — кто вызвал модель.
func (c *Call) Subject() Subject {
	if c == nil {
		return Subject{}
	}
	return c.subject
}

// NextSeq — номер следующего HTTP-вызова (с 1): file call-<n>.
func (c *Call) NextSeq() int {
	if c == nil {
		return 0
	}
	return int(c.seq.Add(1))
}

// AddReasoning добавляет reasoning_content из ответа провайдера.
func (c *Call) AddReasoning(s string) {
	if c == nil || s == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reasoning) >= 64 {
		return
	}
	size := 0
	for _, part := range c.reasoning {
		size += len(part)
	}
	if size >= maxCallReasoningBytes {
		return
	}
	// Один гигантский чанк режем по остатку лимита (по границе рун, чтобы не
	// оставить битый UTF-8).
	if remain := maxCallReasoningBytes - size; len(s) > remain {
		cut := string([]byte(s)[:remain])
		cut = strings.ToValidUTF8(cut, "")
		s = cut
	}
	c.reasoning = append(c.reasoning, s)
}

// TakeReasoning отдаёт накопленный reasoning и очищает накопитель: первая
// попытка хода и repair разбираются отдельно, как у ReasoningCapture.
func (c *Call) TakeReasoning() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reasoning) == 0 {
		return ""
	}
	out := ""
	for _, part := range c.reasoning {
		out += part
	}
	c.reasoning = nil
	return out
}

// TakeReasoning выбирает reasoning: то, что собрал middleware (ago), а если
// ago молчит — накопленное транспортом (40.3). Накопитель очищается в обоих
// случаях, чтобы repair-попытка не получила reasoning первой.
func TakeReasoning(c *Call, captured string) string {
	if captured != "" {
		if c != nil {
			c.TakeReasoning()
		}
		return captured
	}
	return c.TakeReasoning()
}

// callKey — ключ context.Context для *Call.
type callKey struct{}

// WithCall кладёт вызов в контекст: дальше его видят транспорт и вызывающий.
func WithCall(ctx context.Context, c *Call) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, callKey{}, c)
}

// CallFromContext достаёт вызов из контекста (nil — не модельный вызов хода).
func CallFromContext(ctx context.Context) *Call {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(callKey{}).(*Call)
	return c
}
