// Векторизатор записей памяти (спека §10.4 п.4): OpenAI-совместимый endpoint
// из конфигурации (EMBEDDING_*). Ключ не настроен — семантическая ветка
// недоступна (ErrSemanticUnavailable), full-text продолжает работать.
// HashEmbedder — детерминированный фейковый векторизатор для тестов.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Embedder — контракт векторизатора: имя модели (ключ переиндексации в
// memory_embeddings) и пакетное превращение текстов в векторы. Реализации
// должны быть безопасны для параллельных вызовов.
type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenAIEmbedder — клиент POST {endpoint}/embeddings: {"model", "input"}.
// Endpoint — база OpenAI-совместимого API (например https://api.openai.com/v1).
type OpenAIEmbedder struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// NewOpenAIEmbedder строит клиент векторизатора с таймаутом на вызов.
func NewOpenAIEmbedder(endpoint, apiKey, model string, timeout time.Duration, client *http.Client) *OpenAIEmbedder {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &OpenAIEmbedder{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
		client:   client,
	}
}

// Model возвращает имя модели эмбеддингов.
func (e *OpenAIEmbedder) Model() string { return e.model }

// embedRequest/embedResponse — формат OpenAI /v1/embeddings (запрос и ответ).
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed возвращает по вектору на текст, в порядке входа.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embeddings: сериализация запроса: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embeddings: запрос не построен: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: вызов %s: %w", e.endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("embeddings: чтение ответа: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings: ответ не разбирается: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("embeddings: endpoint вернул ошибку: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: получено %d векторов на %d текстов", len(parsed.Data), len(texts))
	}

	out := make([][]float32, len(texts))
	dim := -1
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(out) || out[item.Index] != nil {
			return nil, fmt.Errorf("embeddings: некорректный index %d в ответе", item.Index)
		}
		if dim == -1 {
			dim = len(item.Embedding)
		}
		if len(item.Embedding) != dim {
			return nil, fmt.Errorf("embeddings: разные размерности векторов: %d и %d", dim, len(item.Embedding))
		}
		out[item.Index] = item.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("embeddings: нет вектора для входа %d", i)
		}
	}
	return out, nil
}

// HashEmbedder — детерминированный фейковый векторизатор для тестов
// (§10.4: контракт + фейк, когда ключ не настроен). Текст разбивается на
// 3-граммы, каждая попадает в корзину по FNV-1a; вектор нормируется.
// Похожие тексты получают пересекающиеся корзины — близость осмысленна
// настолько, насколько это нужно тестам; в продуктовую проводку не
// попадает никогда (без EMBEDDING_* семантика выключена целиком).
type HashEmbedder struct {
	Dim   int
	Label string
}

// NewHashEmbedder строит фейк с разумной размерностью по умолчанию.
func NewHashEmbedder() HashEmbedder { return HashEmbedder{Dim: 64, Label: "hash-test-embedder"} }

// Model возвращает имя фейка.
func (h HashEmbedder) Model() string { return h.Label }

// Embed считает детерминированные векторы.
func (h HashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = h.embedOne(text)
	}
	return out, nil
}

func (h HashEmbedder) embedOne(text string) []float32 {
	dim := h.Dim
	if dim <= 0 {
		dim = 64
	}
	vec := make([]float32, dim)
	runes := []rune(strings.ToLower(text))
	for i := 0; i+3 <= len(runes); i++ {
		hsh := fnv.New32a()
		_, _ = hsh.Write([]byte(string(runes[i : i+3])))
		vec[hsh.Sum32()%uint32(dim)]++
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		vec[0] = 1 // пустой/короткий текст — детерминированная единичная корзина
		return vec
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

// vectorLiteral рендерит вектор в текстовый литерал pgvector '[a,b,c]'.
func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
