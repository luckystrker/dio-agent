//go:build db

// Тест-каркас E2E (этап 9.7): приложение целиком — poller → ingest → буферы →
// когнитивный worker → применение → доставка, плюс фон — на скриптованной
// офлайн-модели (agotesting) и заглушке Bot API. Отдельная чистая база на
// прогон — создать, применить схему (7 миграций), удалить после теста. Запуск:
//
//	go test -tags db ./internal/app/
//
// против Postgres из docker-compose; TEST_DATABASE_URL переопределяет DSN
// (на машине разработчика Postgres docker-compose занимает 5433 — см. 9.1).
// Недоступная база — t.Skip с понятной причиной, юнит-гейт это не задевает.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"

	"github.com/luckystrker/dio-agent/internal/background"
	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/internal/telegram"
	"github.com/luckystrker/dio-agent/internal/worker"
	"github.com/luckystrker/dio-agent/migrations"
)

const defaultTestDSN = "postgres://dio:dio@localhost:5432/dio?sslmode=disable"

// --- заглушка Bot API ---------------------------------------------------------

// stubCall — журнал вызовов Bot API (что именно ушло в Telegram).
type stubCall struct {
	Method    string
	ChatID    int64
	Text      string
	Emoji     string
	MessageID int64
}

// stubAPI — программируемый Bot API: пачка updates на выдачу, поведенческий
// крючок sendMessage (успех/ошибка/зависание — для политики §13.2).
type stubAPI struct {
	mu      sync.Mutex
	updates []telegram.Update
	calls   []stubCall
	msgID   int64
	// onSendMessage — программируемый исход sendMessage; nil — успех.
	onSendMessage func(chatID int64, text string) (int64, error)
}

func newStubAPI() *stubAPI { return &stubAPI{msgID: 1000} }

var _ telegram.API = (*stubAPI)(nil)

// push добавляет update в очередь выдачи (нормальный JSON сообщения);
// актор по умолчанию — Коля (id 777).
func (s *stubAPI) push(updateID, chatID int64, chatType, text string) {
	s.pushAs(updateID, chatID, chatType, 777, "Коля", text)
}

// pushAs — то же с явным актором (эталоны поведения: незнакомец ≠ свой).
func (s *stubAPI) pushAs(updateID, chatID int64, chatType string, actorID int64, actorName, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgID++
	raw := fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":%d,"date":1,"text":%q,`+
			`"from":{"id":%d,"first_name":%q},"chat":{"id":%d,"type":%q}}}`,
		updateID, s.msgID, text, actorID, actorName, chatID, chatType)
	s.updates = append(s.updates, telegram.Update{UpdateID: updateID, Raw: json.RawMessage(raw)})
}

func (s *stubAPI) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Повтор пачки с прежнего offset должен вернуть те же updates (§6.1):
	// выданные остаются в очереди, пока poller не двинет offset за них.
	var out []telegram.Update
	for _, u := range s.updates {
		if u.UpdateID >= offset {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *stubAPI) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	s.mu.Lock()
	hook := s.onSendMessage
	if hook == nil {
		s.msgID++
		id := s.msgID
		s.calls = append(s.calls, stubCall{Method: "sendMessage", ChatID: chatID, Text: text, MessageID: id})
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()
	id, err := hook(chatID, text)
	if err == nil {
		s.mu.Lock()
		s.calls = append(s.calls, stubCall{Method: "sendMessage", ChatID: chatID, Text: text, MessageID: id})
		s.mu.Unlock()
	}
	return id, err
}

func (s *stubAPI) SendChatAction(ctx context.Context, chatID int64, action string) error {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{Method: "sendChatAction", ChatID: chatID})
	s.mu.Unlock()
	return nil
}

func (s *stubAPI) SetMessageReaction(ctx context.Context, chatID int64, messageID int64, emoji string) error {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{Method: "setMessageReaction", ChatID: chatID, MessageID: messageID, Emoji: emoji})
	s.mu.Unlock()
	return nil
}

// calls возвращает копию журнала вызовов.
func (s *stubAPI) callsOf(method string) []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubCall
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// --- часы планировщика ---------------------------------------------------------

// clock — управляемые часы планировщика фона (двигаем дни, §1).
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Now().UTC()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// --- окружение ----------------------------------------------------------------

// harness — приложение целиком на чистой тестовой базе.
type harness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	dsn    string
	api    *stubAPI
	script *agotest.TestConfig
	clk    *clock
	app    *App
}

// fastWindows — короткие окна буферов (E2E не ждёт секунды).
func fastWindows(telegram.ChatType, telegram.EventKind) telegram.Windows {
	return telegram.Windows{Idle: 25 * time.Millisecond, Max: 60 * time.Millisecond}
}

// newHarness собирает приложение: чистая база, скриптовая модель (resp —
// глобальная очередь ответов всех модельных вызовов прогонов), заглушка API.
func newHarness(t *testing.T, resp ...*events.ModelResponse) *harness {
	t.Helper()
	return newHarnessTuned(t, nil, resp...)
}

// newHarnessTuned — то же с настройкой Options до старта (устойчивость 9.7:
// свой векторизатор, интервалы циклов и т.п.).
func newHarnessTuned(t *testing.T, tune func(*Options), resp ...*events.ModelResponse) *harness {
	t.Helper()
	dsn := newTestDB(t)
	return newHarnessDSNTuned(t, dsn, tune, resp...)
}

// newTestDB создаёт чистую тестовую базу и возвращает её DSN (перезапуск
// процесса 9.7: два приложения на одну базу — например, recovery после сбоя).
func newTestDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	defer admin.Close(ctx)

	dbName := fmt.Sprintf("dio_app_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	})

	testDSN, err := swapDatabaseInDSNString(dsn, dbName)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL не разобран как URL: %v", err)
	}
	return testDSN
}

// poolFor открывает пул к базе по DSN (второй «процесс»).
func poolFor(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := runtime.NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("пул к тестовой базе не создан: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newHarnessDSN — приложение на готовой базе (перезапуск: та же база,
// свежий пул и скрипт модели).
func newHarnessDSN(t *testing.T, dsn string, resp ...*events.ModelResponse) *harness {
	t.Helper()
	return newHarnessDSNTuned(t, dsn, nil, resp...)
}

// newHarnessDSNTuned — приложение на готовой базе с настройкой Options до старта.
func newHarnessDSNTuned(t *testing.T, dsn string, tune func(*Options), resp ...*events.ModelResponse) *harness {
	t.Helper()

	pool := poolFor(t, dsn)
	api := newStubAPI()
	script := agotest.NewTestConfig(resp...)
	clk := newClock()

	opts := Options{
		Config: runtime.Config{
			DatabaseURL: dsn,
			LogLevel:    runtime.LogLevelError,
			NodeEnv:     runtime.NodeEnvTest,
		},
		// §7.1-файлы из корня репозитория: дефолтные пути относительны рабочей
		// директории процесса, тесты работают из internal/app.
		PersonaFile:    "../../config/persona.md",
		PromptTemplate: "../../config/system-prompt.md",
		Logger:         log.New(io.Discard, "", 0),
		Model:          character.NewChatConfigWithClient(script),
		Telegram:       api,
		Now:            clk.Now,
		SingletonWatch: 20 * time.Millisecond,
		DrainLimit:     10 * time.Second,
		Buffers:        fastWindows,
		Poll:           telegram.PollerOptions{PollTimeout: 30 * time.Millisecond, ErrorBackoff: 20 * time.Millisecond},
		Cognitive:      worker.Options{PollInterval: 20 * time.Millisecond, RetryBase: 30 * time.Millisecond, RetryMax: 120 * time.Millisecond},
		Delivery:       telegram.DeliveryOptions{Interval: 20 * time.Millisecond, BubbleGap: 2 * time.Millisecond, RetryBase: 30 * time.Millisecond},
		Background:     background.Options{MinInterval: 50 * time.Millisecond, MaxInterval: 50 * time.Millisecond, RetryBackoff: 50 * time.Millisecond},
	}
	if tune != nil {
		tune(&opts)
	}

	a, err := New(opts)
	if err != nil {
		t.Fatalf("приложение не собрано: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("приложение не запущено: %v", err)
	}
	t.Cleanup(func() {
		a.Stop(context.Background())
	})

	return &harness{t: t, pool: pool, dsn: dsn, api: api, script: script, clk: clk, app: a}
}

// hardKill — имитация SIGKILL: отмена контекста, закрытие пула и обрыв
// singleton-соединения — всё без graceful-ожиданий (§5.3: recovery обязан
// работать и без graceful; замок снимается разрывом соединения, §5.1).
func (h *harness) hardKill() {
	h.app.cancelIntake()
	h.app.cancelRun()
	if h.app.single != nil {
		h.app.single.Drop()
	}
	if h.app.pool != nil {
		h.app.pool.Close()
	}
}

// modelCalls — сколько модельных вызовов сделано скриптовым дублем.
func (h *harness) modelCalls() int { return len(h.script.Calls()) }

// --- ожидания (poll с дедлайном — асинхронные циклы) ---------------------------

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("условие не выполнено за 10 с: %s", what)
}

// --- чтение БД -----------------------------------------------------------------

func queryInt(h *harness, sql string, args ...any) int64 {
	h.t.Helper()
	var v int64
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		h.t.Fatalf("запрос %q: %v", sql, err)
	}
	return v
}

func queryString(h *harness, sql string, args ...any) string {
	h.t.Helper()
	var v string
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		h.t.Fatalf("запрос %q: %v", sql, err)
	}
	return v
}

// queryStringOK — как queryString, но «строки нет» — не фатально (опрос
// состояния, которого ещё может не быть).
func queryStringOK(h *harness, sql string, args ...any) (string, bool) {
	h.t.Helper()
	var v string
	err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false
		}
		h.t.Fatalf("запрос %q: %v", sql, err)
	}
	return v, true
}

func queryNilableString(h *harness, sql string, args ...any) *string {
	h.t.Helper()
	var v *string
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		h.t.Fatalf("запрос %q: %v", sql, err)
	}
	return v
}

// --- помощники сценариев ---------------------------------------------------------

// newTestPool создаёт пул к свежей тестовой базе с полной схемой §18
// (копия помощника 9.2–9.6: пакеты не зависят от внутренностей чужих тестов).
func newTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	defer admin.Close(ctx)

	dbName := fmt.Sprintf("dio_app_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	})

	testDSN, err := swapDatabaseInDSN(dsn, dbName)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL не разобран как URL: %v", err)
	}

	pool, err := runtime.NewPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул к тестовой базе не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	result, err := runtime.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("схема не применена: %v", err)
	}
	if result.Applied != 10 || result.Skipped != 0 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу 10/0", result.Applied, result.Skipped)
	}
	return pool, testDSN
}

// swapDatabaseInDSN подменяет имя базы в URL (копия помощника 9.2–9.6).
func swapDatabaseInDSN(dsn, dbName string) (string, error) {
	return swapDatabaseInDSNString(dsn, dbName)
}

func swapDatabaseInDSNString(dsn, dbName string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName), nil
}
