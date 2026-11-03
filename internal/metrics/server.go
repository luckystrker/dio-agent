// HTTP-сервер метрик (этап 31.1): только 127.0.0.1, порт из METRICS_PORT.
// /metrics — Prometheus text exposition, /stats — читаемый JSON, /healthz —
// проверка живости (пинг базы). Снимок кэшируется на 10–30 секунд: БД не
// дёргается на каждый скрейп, а сбой сбора отдаёт последний удачный снимок.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultCacheTTL — срок жизни снимка (этап 31: 10–30 секунд).
const DefaultCacheTTL = 15 * time.Second

// shutdownTimeout — граница graceful shutdown HTTP-сервера.
const shutdownTimeout = 5 * time.Second

// Pinger — минимальный контракт пула для /healthz (*runtime.Pool подходит).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Config — сборка сервера метрик.
type Config struct {
	Port     int // 0 — сервер выключен
	CacheTTL time.Duration
	Logger   *log.Logger
	Source   Source
	DB       Pinger
	Now      func() time.Time
}

// Server — сервер метрик с кэшем снимка.
type Server struct {
	cfg    Config
	logger *log.Logger
	now    func() time.Time
	ttl    time.Duration

	startedAt time.Time

	mu           sync.Mutex
	cached       Snapshot
	cachedAt     time.Time
	scrapeErrors int64

	ln   net.Listener
	http *http.Server
}

// New собирает сервер. Порт 0 допустим (выключен) — тогда Start ничего не
// открывает; источник снимка обязателен.
func New(cfg Config) (*Server, error) {
	if cfg.Source == nil {
		return nil, errors.New("метрики: источник снимка не задан")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Server{cfg: cfg, logger: logger, now: now, ttl: ttl, startedAt: now()}, nil
}

// Start открывает слушатель на 127.0.0.1:Port. Порт занят — ошибка запуска:
// оператор должен увидеть причину, а не молчаливое отсутствие метрик.
func (s *Server) Start() error {
	if s.cfg.Port <= 0 {
		return nil
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("метрики: 127.0.0.1:%d не слушается: %w", s.cfg.Port, err)
	}
	s.ln = ln
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	return nil
}

// Addr — фактический адрес слушателя (пусто, если сервер выключен).
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Run обслуживает запросы до отмены ctx, затем корректно закрывает сервер.
// Вызывается в горутине приложения; ошибки обслуживания только логируются
// (метрики — вспомогательный контур, они не роняют персонажа).
func (s *Server) Run(ctx context.Context) {
	if s.ln == nil {
		<-ctx.Done()
		return
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(s.ln) }()
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Printf("метрики: обслуживание остановлено: %v", err)
			return
		}
	}
	s.Shutdown()
}

// Shutdown останавливает HTTP-сервер в пределах shutdownTimeout. Повторный
// вызов и вызов до Start безопасны.
func (s *Server) Shutdown() error {
	if s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(ctx)
	s.http = nil
	s.ln = nil
	return err
}

// Handler — маршруты сервера. Только GET: на прочие методы отдаётся 405
// (ServeMux Go 1.22+ сам знает, что путь существует).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		http.Error(w, "метрики: снимок недоступен: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		s.logger.Printf("метрики: /stats не записан: %v", err)
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		http.Error(w, "метрики: снимок недоступен: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := WritePrometheus(w, snap); err != nil {
		s.logger.Printf("метрики: /metrics не записан: %v", err)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	db := "ok"
	if s.cfg.DB != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.cfg.DB.Ping(ctx); err != nil {
			status, db, code = "degraded", "error", http.StatusServiceUnavailable
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         status,
		"db":             db,
		"uptime_seconds": s.now().Sub(s.startedAt).Seconds(),
	})
}

// snapshot отдаёт кэш в пределах TTL или собирает новый. Сбой сбора после
// удачного снимка не превращается в 500: отдаётся последний снимок, ошибка
// считается (наблюдаемость самого сервера метрик).
func (s *Server) snapshot(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.cachedAt.IsZero() && now.Sub(s.cachedAt) < s.ttl {
		return s.decorate(s.cached, now, now.Sub(s.cachedAt)), nil
	}
	snap, err := s.cfg.Source.Snapshot(ctx)
	if err != nil {
		s.scrapeErrors++
		if !s.cachedAt.IsZero() {
			s.logger.Printf("метрики: сбор не удался, отдаю кэш %s давности: %v",
				now.Sub(s.cachedAt).Round(time.Second), err)
			return s.decorate(s.cached, now, now.Sub(s.cachedAt)), nil
		}
		return Snapshot{}, err
	}
	s.cached = snap
	s.cachedAt = now
	return s.decorate(snap, now, 0), nil
}

// decorate добавляет состояние самого сервера (единственные счётчики
// процесса: в базе им места нет) и не трогает кэшированные карты.
func (s *Server) decorate(snap Snapshot, now time.Time, cacheAge time.Duration) Snapshot {
	snap.Server = ServerStats{
		ScrapeErrors:    s.scrapeErrors,
		CacheAgeSeconds: cacheAge.Seconds(),
		UptimeSeconds:   now.Sub(s.startedAt).Seconds(),
	}
	return snap
}
