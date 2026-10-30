// Singleton (§5.1): PostgreSQL session advisory lock на отдельном постоянном
// соединении. Занятый замок — причина не запускаться (второй процесс выходит
// с ошибкой). Потеря singleton-соединения прекращает работу: продолжать без
// singleton-защиты нельзя, процесс завершается под перезапуск systemd.
// Поведение-справочник — src/runtime/singleton.ts.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// SingletonLockKey — ключ advisory-замка, константа на всё приложение
// («dio» в hex, паритет TS).
const SingletonLockKey = 0x64_69_6f

// ErrSingletonTaken — замок занят: другой экземпляр процесса уже работает
// с этой базой. Явная причина не запускаться (§5.1).
var ErrSingletonTaken = errors.New("singleton: замок занят — другой экземпляр процесса уже работает с этой базой (§5.1)")

// DefaultWatchInterval — период проверки живости singleton-соединения.
const DefaultWatchInterval = 5 * time.Second

// Singleton — удерживаемый замок одного живого процесса.
type Singleton struct {
	conn *pgx.Conn
	pid  int64 // pg_backend_pid: диагностика и тесты (pg_terminate_backend)

	mu        sync.Mutex
	lost      func(reason string)
	watchStop chan struct{}
	watchDone chan struct{}
	released  bool
}

// AcquireSingleton берёт session advisory lock на новом соединении.
// Замок занят — ErrSingletonTaken, соединение закрывается.
func AcquireSingleton(ctx context.Context, dsn string) (*Singleton, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("singleton: соединение не открыто: %w", err)
	}

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", SingletonLockKey).Scan(&locked); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("singleton: замок не проверяется: %w", err)
	}
	if !locked {
		_ = conn.Close(context.Background())
		return nil, ErrSingletonTaken
	}

	var pid int64
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("singleton: pid соединения не читается: %w", err)
	}

	return &Singleton{
		conn:      conn,
		pid:       pid,
		watchStop: make(chan struct{}),
		watchDone: make(chan struct{}),
	}, nil
}

// PID возвращает backend pid соединения-замка (диагностика, тесты).
func (s *Singleton) PID() int64 { return s.pid }

// OnLost регистрирует реакцию на потерю singleton-соединения. Вызывается
// однократно; продолжать работу без замка нельзя (§5.1) — вызывающий обязан
// остановить процесс.
func (s *Singleton) OnLost(lost func(reason string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost = lost
}

// StartWatch запускает фоновую проверку живости соединения (pgx не отдаёт
// событий разрыва — периодический ping). interval <= 0 — DefaultWatchInterval.
func (s *Singleton) StartWatch(interval time.Duration) {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	go func() {
		defer close(s.watchDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.watchStop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				err := s.conn.Ping(ctx)
				cancel()
				if err == nil {
					continue
				}
				s.reportLost(fmt.Sprintf("singleton-соединение потеряно: %v", err))
				return
			}
		}
	}()
}

// reportLost единожды сообщает о потере и закрывает соединение.
func (s *Singleton) reportLost(reason string) {
	s.mu.Lock()
	cb := s.lost
	s.released = true
	s.mu.Unlock()
	_ = s.conn.Close(context.Background())
	if cb != nil {
		cb(reason)
	}
}

// Drop обрывает соединение без явного unlock — как при падении процесса:
// session lock снимает сервер при разрыве соединения. Штатный shutdown —
// Release.
func (s *Singleton) Drop() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	s.mu.Unlock()
	select {
	case <-s.watchStop:
	default:
		close(s.watchStop)
	}
	_ = s.conn.Close(context.Background())
}

// Release снимает замок и закрывает соединение (shutdown). Повторный вызов
// и вызов после потери — безопасны (no-op).
func (s *Singleton) Release(ctx context.Context) error {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return nil
	}
	s.released = true
	s.mu.Unlock()

	select {
	case <-s.watchStop:
	default:
		close(s.watchStop)
	}
	select {
	case <-s.watchDone:
	case <-time.After(2 * time.Second):
	}

	// Разрыв соединения снимает session lock сам; явный unlock — паритет TS
	// и страховка на случай переиспользования соединения сервером.
	_, _ = s.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", SingletonLockKey)
	return s.conn.Close(ctx)
}
