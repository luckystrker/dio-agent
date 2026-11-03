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

// pingFailuresBeforeLoss — сколько подряд неудач Ping считаются потерей:
// одна неудача (GC-пауза, микросброс сети) — не потеря, ложный рестарт
// посреди хода роняет ответ и платит дважды.
const pingFailuresBeforeLoss = 3

// Singleton — удерживаемый замок одного живого процесса.
type Singleton struct {
	conn *pgx.Conn
	dsn  string // DSN соединения-замка: проверка «БД жива?» новым соединением (18.11)
	pid  int64  // pg_backend_pid: диагностика и тесты (pg_terminate_backend)

	// Швы наблюдения для тестов (18.11): переопределяются до StartWatch.
	// ping — живость соединения-замка (по умолчанию conn.Ping): тесту нужен
	// паттерн «неудача-успех-неудача», который на живом соединении не
	// воспроизвести. verify — «БД отвечает новому соединению?» (по умолчанию
	// dbReachable из того же DSN).
	ping   func(ctx context.Context) error
	verify func() bool

	mu        sync.Mutex
	lost      func(reason string)
	watchStop chan struct{}
	watchDone chan struct{}
	released  bool
	// connMu сериализует обращения к conn: наблюдатель мог зависнуть в ping
	// дольше Release-ожидания, и конкурентный Exec/Close по pgx.Conn
	// недопустим (minor 8).
	connMu sync.Mutex
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
		dsn:       dsn,
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
// Один неудачный Ping — не потеря: доклад о потере требует 3 подряд неудачи
// (успех сбрасывает счётчик) и проверку на НОВОМ соединении из того же DSN:
// БД отвечает, а замок-соединение молчит — замок потерян по-настоящему
// (соединение оборвано, сервер снял session lock); БД тоже не отвечает —
// сеть, ждём восстановления и не валимся.
func (s *Singleton) StartWatch(interval time.Duration) {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	// Швы тестов: без переопределения — настоящие Ping и dbReachable.
	ping := s.ping
	if ping == nil {
		ping = s.conn.Ping
	}
	pingLocked := ping
	ping = func(ctx context.Context) error {
		s.connMu.Lock()
		defer s.connMu.Unlock()
		return pingLocked(ctx)
	}
	verify := s.verify
	if verify == nil {
		verify = func() bool { return dbReachable(s.dsn, interval) }
	}
	go func() {
		defer close(s.watchDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-s.watchStop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				err := ping(ctx)
				cancel()
				if err == nil {
					failures = 0
					continue
				}
				failures++
				if failures < pingFailuresBeforeLoss {
					continue
				}
				if !verify() {
					// БД недоступна и новому соединению — сеть: замок, если
					// жив, никуда не делся, ждём дальше (не валимся).
					continue
				}
				s.reportLost(fmt.Sprintf("singleton-соединение потеряно: %v (БД отвечает новому соединению — замок снят сервером)", err))
				return
			}
		}
	}()
}

// dbReachable отвечает, отвечает ли БД НОВОМУ соединению из dsn (таймаут —
// как интервал наблюдения): отличает «замок потерян» от «сеть пропала».
func dbReachable(dsn string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false
	}
	defer conn.Close(context.Background())
	return conn.Ping(ctx) == nil
}

// reportLost единожды сообщает о потере и закрывает соединение.
func (s *Singleton) reportLost(reason string) {
	s.mu.Lock()
	if s.released {
		// Release уже прошёл, пока наблюдение сидело в verify: докладывать
		// потерю после штатного shutdown нельзя.
		s.mu.Unlock()
		return
	}
	cb := s.lost
	s.released = true
	s.mu.Unlock()
	s.connMu.Lock()
	_ = s.conn.Close(context.Background())
	s.connMu.Unlock()
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
	s.connMu.Lock()
	_ = s.conn.Close(context.Background())
	s.connMu.Unlock()
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
	s.connMu.Lock()
	defer s.connMu.Unlock()
	_, _ = s.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", SingletonLockKey)
	return s.conn.Close(ctx)
}
