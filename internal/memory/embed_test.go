// Юнит-тесты векторизатора: контракт OpenAI-совместимого endpoint через
// httptest (без живой сети), фейковый HashEmbedder для тестов без ключа.
package memory

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHashEmbedderDeterministicAndNormalized(t *testing.T) {
	emb := NewHashEmbedder()
	vecs, err := emb.Embed(context.Background(), []string{"новый год", "новый год", "кофе утром"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("векторов = %d, хочу 3", len(vecs))
	}
	if len(vecs[0]) != emb.Dim {
		t.Fatalf("размерность = %d, хочу %d", len(vecs[0]), emb.Dim)
	}
	for i := range vecs[0] {
		if vecs[0][i] != vecs[1][i] {
			t.Fatalf("одинаковые тексты дали разные векторы")
		}
	}
	for _, v := range vecs {
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if math.Abs(norm-1) > 1e-5 {
			t.Fatalf("норма вектора = %v, хочу 1", norm)
		}
	}
	if emb.Model() == "" {
		t.Fatal("фейк без имени модели не ключуется в memory_embeddings")
	}
}

func TestVectorLiteral(t *testing.T) {
	got := vectorLiteral([]float32{0.5, -1.25, 2})
	if got != "[0.5,-1.25,2]" {
		t.Errorf("vectorLiteral = %q, хочу %q", got, "[0.5,-1.25,2]")
	}
}

// httptest-сервер, отвечающий как OpenAI /v1/embeddings и записывающий запрос.
func embeddingsServer(t *testing.T, status int, body string, seen *map[string]any, auth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("путь %q, хочу .../embeddings", r.URL.Path)
		}
		*auth = r.Header.Get("Authorization")
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("запрос не разбирается: %v", err)
		}
		*seen = payload
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestOpenAIEmbedderHappyPath(t *testing.T) {
	var payload map[string]any
	var auth string
	server := embeddingsServer(t, http.StatusOK,
		`{"data":[{"index":1,"embedding":[0.4,0.2]},{"index":0,"embedding":[0.1,0.9]}]}`,
		&payload, &auth)
	defer server.Close()

	emb := NewOpenAIEmbedder(server.URL, "sk-test", "emb-model", time.Second, server.Client())
	got, err := emb.Embed(context.Background(), []string{"первый", "второй"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, хочу Bearer sk-test", auth)
	}
	if payload["model"] != "emb-model" {
		t.Errorf("model в запросе = %v", payload["model"])
	}
	input, _ := payload["input"].([]any)
	if len(input) != 2 || input[0] != "первый" || input[1] != "второй" {
		t.Errorf("input в запросе = %v", payload["input"])
	}
	// Порядок ответа восстанавливается по index, не по порядку следования.
	if len(got) != 2 || got[0][0] != 0.1 || got[1][0] != 0.4 {
		t.Errorf("векторы = %v, ожидаю восстановление по index", got)
	}
	if emb.Model() != "emb-model" {
		t.Errorf("Model() = %q", emb.Model())
	}
}

func TestOpenAIEmbedderErrors(t *testing.T) {
	ctx := context.Background()
	newEmbedder := func(server *httptest.Server) *OpenAIEmbedder {
		return NewOpenAIEmbedder(server.URL, "k", "m", time.Second, server.Client())
	}

	t.Run("HTTP 500 — честная ошибка с телом", func(t *testing.T) {
		var seen map[string]any
		var auth string
		server := embeddingsServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &seen, &auth)
		defer server.Close()
		if _, err := newEmbedder(server).Embed(ctx, []string{"x"}); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v, ожидаю HTTP 500", err)
		}
	})

	t.Run("не хватает векторов", func(t *testing.T) {
		var seen map[string]any
		var auth string
		server := embeddingsServer(t, http.StatusOK, `{"data":[{"index":0,"embedding":[1,2]}]}`, &seen, &auth)
		defer server.Close()
		if _, err := newEmbedder(server).Embed(ctx, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "1 векторов на 2") {
			t.Fatalf("err = %v, ожидаю несовпадение числа векторов", err)
		}
	})

	t.Run("разные размерности", func(t *testing.T) {
		var seen map[string]any
		var auth string
		server := embeddingsServer(t, http.StatusOK,
			`{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[1,2,3]}]}`, &seen, &auth)
		defer server.Close()
		if _, err := newEmbedder(server).Embed(ctx, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "размерност") {
			t.Fatalf("err = %v, ожидаю ошибку размерности", err)
		}
	})

	t.Run("пустой вход — пустой выход без вызова", func(t *testing.T) {
		var seen map[string]any
		var auth string
		server := embeddingsServer(t, http.StatusOK, `{"data":[]}`, &seen, &auth)
		defer server.Close()
		got, err := newEmbedder(server).Embed(ctx, nil)
		if err != nil || got != nil {
			t.Fatalf("Embed(nil) = %v, %v; хочу nil, nil", got, err)
		}
		if auth != "" {
			t.Errorf("пустой вход не должен ходить в сеть, но запрос ушёл")
		}
	})
}
