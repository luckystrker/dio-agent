//go:build db

// БД-тест доставки картинки (этап 36.2): байты лежат в outbox_images, delivery
// отправляет их sendPhoto (multipart), подтверждённый ответ — sent; пропавшие
// байты — терминальный failed, а не зависание очереди.
package telegram

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// photoStub — Bot API-заглушка sendPhoto: запоминает файл и отвечает message_id.
type photoStub struct {
	mu        sync.Mutex
	sent      int
	filename  string
	fileType  string
	fileBytes []byte
}

func (s *photoStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTESTTOKEN/sendPhoto" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("content-type"))
		if err != nil {
			t.Errorf("sendPhoto без multipart: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("multipart не разобран: %v", err)
				break
			}
			if part.FormName() != "photo" {
				continue
			}
			s.mu.Lock()
			s.filename = part.FileName()
			s.fileType = part.Header.Get("content-type")
			s.fileBytes, _ = io.ReadAll(part)
			s.sent++
			s.mu.Unlock()
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":888}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *photoStub) snapshot() (int, string, string, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent, s.filename, s.fileType, append([]byte(nil), s.fileBytes...)
}

func TestDeliveryLoopSendsPhoto(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &photoStub{}
	srv := stub.server(t)
	client := NewClient("TESTTOKEN", srv.URL, srv.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 200, 42, 1, `{"kind":"photo","source_url":"https://cdn.example.com/a.jpg"}`)
	if _, err := pool.Exec(ctx,
		`INSERT INTO outbox_images (outbox_id, content_type, bytes) VALUES ($1, 'image/jpeg', $2)`,
		id, []byte("jpeg-байты")); err != nil {
		t.Fatalf("байты картинки не вставлены: %v", err)
	}

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{BubbleGap: time.Millisecond})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var (
		status     string
		telegramID *int64
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, telegram_message_id FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &telegramID); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sent" || telegramID == nil || *telegramID != 888 {
		t.Fatalf("доставка фото: status=%s id=%v", status, telegramID)
	}
	sent, name, fileType, data := stub.snapshot()
	if sent != 1 || name != "photo.jpg" || fileType != "image/jpeg" || string(data) != "jpeg-байты" {
		t.Fatalf("sendPhoto: вызовов=%d файл=%q тип=%q байты=%q", sent, name, fileType, data)
	}
}

func TestDeliveryLoopPhotoWithoutBytesFails(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &photoStub{}
	srv := stub.server(t)
	client := NewClient("TESTTOKEN", srv.URL, srv.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 201, 42, 0, `{"kind":"photo","source_url":"https://cdn.example.com/a.jpg"}`)
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{BubbleGap: time.Millisecond})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "failed" {
		t.Fatalf("картинка без байтов: status=%q, хочу failed", status)
	}
	if sent, _, _, _ := stub.snapshot(); sent != 0 {
		t.Fatalf("sendPhoto без байтов не должен вызываться (вызовов %d)", sent)
	}
}
