//go:build db

// Общий помощник БД-тестов фона (этап 9.6): отдельная чистая база на прогон —
// создать, применить схему (7 миграций), удалить после теста. Запуск:
//
//	go test -tags db ./internal/background/
//
// против Postgres из docker-compose; TEST_DATABASE_URL переопределяет DSN
// (на машине разработчика Postgres docker-compose занимает 5433 — см. 9.1).
// Недоступная база — t.Skip с понятной причиной, юнит-гейт это не задевает.
package background

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ago/events"
	agotest "ago/testing"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

const defaultTestDSN = "postgres://dio:dio@localhost:5432/dio?sslmode=disable"

const (
	bgChatID int64 = -200700
	bgScope        = "chat:-200700"
	bgActor  int64 = 888
)

// fixture — готовый фон: схема, персона, журнал, WAL, чекпоинты, бюджет.
type fixture struct {
	pool        *pgxpool.Pool
	journal     *character.TurnJournal
	wal         *runtime.TaskWAL
	checkpoints *runtime.PgCheckpointStore
	budget      *runtime.BudgetStore
	intentions  *IntentionRepo
	sched       *Scheduler
	now         time.Time // управляемые часы планировщика
}

// newFixture собирает окружение фона на чистой тестовой базе.
func newFixture(t *testing.T) (*fixture, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	persona, err := (character.NewPersonaRepo()).Active(ctx, pool)
	if err != nil {
		t.Fatalf("персона не создана: %v", err)
	}

	toolset, err := memory.NewToolset(pool, &memory.Searcher{Embedder: nil, Items: memory.Items{}})
	if err != nil {
		t.Fatalf("toolset памяти не создан: %v", err)
	}

	storage := runtime.NewPgStreamStorage(ctx, pool)
	f := &fixture{
		pool:        pool,
		journal:     character.NewTurnJournal(storage, nil),
		wal:         runtime.NewTaskWAL(storage),
		checkpoints: runtime.NewPgCheckpointStore(ctx, pool),
		budget:      runtime.NewBudgetStore(pool),
		intentions:  NewIntentionRepo(pool),
		// Часы — реальные, с возможностью сдвига (setNow): гейты сравнивают
		// их с DB-временем (created_at/updated_at), расхождение баз должно
		// быть миллисекундным.
		now: time.Now().UTC(),
	}

	sched, err := New(Deps{
		Pool:            pool,
		Journal:         f.journal,
		WAL:             f.wal,
		Checkpoints:     f.checkpoints,
		Budget:          f.budget,
		Memory:          toolset,
		Persona:         persona,
		DailyCallsLimit: 1000,
		ToolCallsLimit:  5,
		Logger:          log.New(os.Stderr, "", 0),
		Now:             func() time.Time { return f.now },
	}, Options{})
	if err != nil {
		t.Fatalf("планировщик не создан: %v", err)
	}
	f.sched = sched
	return f, ctx
}

// withModel подключает скриптовую модель (дубль agotesting): ответы выдаются
// по порядку, исчерпание — ошибка (лишний модельный вызвал валит тест).
func (f *fixture) withModel(t *testing.T, resp ...*events.ModelResponse) *agotest.TestConfig {
	t.Helper()
	cfg := agotest.NewTestConfig(resp...)
	f.sched.model = character.NewChatConfigWithClient(cfg)
	return cfg
}

func (f *fixture) setNow(at time.Time) { f.now = at }

// createIntention вставляет намерение через общий путь применения (§14.1):
// dedupe, notBefore/expires, provenance — ровно как из хода.
func (f *fixture) createIntention(t *testing.T, ctx context.Context, kind, topic string, delayHours float64, chatID int64) int64 {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	id, created, err := character.CreateIntentionTx(ctx, tx, character.IntentionParams{
		Kind: kind, ChatID: chatID, ScopeID: memory.ChatScope(fmt.Sprintf("%d", chatID)),
		Topic: topic, Reason: "тест 9.6: " + topic, DelayHours: delayHours,
	})
	if err != nil {
		t.Fatalf("намерение не создано: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("транзакция не зафиксирована: %v", err)
	}
	if !created {
		t.Fatalf("намерение %q не создано (dedupe сработал неожиданно)", topic)
	}
	return id
}

// seedEventStatus — событие чата с заданным статусом (актор, тип, время;
// уникальный update_id). processed — материал консолидации; journal_only —
// фон чата, который не должен скармливаться эпизодам.
func (f *fixture) seedEventStatus(t *testing.T, ctx context.Context, chatID int64, chatType, text string, at time.Time, status string) int64 {
	t.Helper()
	var id int64
	err := f.pool.QueryRow(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, occurred_at, telegram_message_id)
		VALUES ($1::bigint, $2::bigint, $3::text, $4::bigint, 'Коля', 'message', $5::text,
		        jsonb_build_object('message', jsonb_build_object('message_id', $6::bigint, 'text', $5::text, 'from', jsonb_build_object('id', $4::bigint))),
		        $7::text, $8, $6::bigint)
		RETURNING id`,
		f.nextUpdateID(), chatID, chatType, bgActor, text, 7000+f.nextMessageID(), status, at).Scan(&id)
	if err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}
	return id
}

// seedEvent — processed-событие (материал эпизодов консолидации).
func (f *fixture) seedEvent(t *testing.T, ctx context.Context, chatID int64, chatType, text string, at time.Time) int64 {
	t.Helper()
	return f.seedEventStatus(t, ctx, chatID, chatType, text, at, "processed")
}

// seedPrivateChat — событие личного чата вне консолидации (journal_only):
// тесты намерений не должны задевать эпизодный контур.
func (f *fixture) seedPrivateChat(t *testing.T, ctx context.Context, text string, at time.Time) int64 {
	t.Helper()
	return f.seedEventStatus(t, ctx, bgChatID, "private", text, at, "journal_only")
}

// seedEpisode — эпизод: n processed-событий + применённый ход + запись памяти
// с сущностью «Коля» (материал консолидации: transcript, словарь имён, basis).
func (f *fixture) seedEpisode(t *testing.T, ctx context.Context, chatID int64, chatType string, n int, lastAt time.Time) ([]int64, int64) {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		at := lastAt.Add(-time.Duration(n-i) * time.Minute)
		id := f.seedEvent(t, ctx, chatID, chatType, fmt.Sprintf("сообщение эпизода %d: разговор про виниловые пластинки", i), at)
		ids = append(ids, id)
	}

	var turnID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO turns (chat_id, status, applied_at) VALUES ($1, 'applied', now()) RETURNING id`, chatID).Scan(&turnID); err != nil {
		t.Fatalf("ход эпизода не создан: %v", err)
	}
	for _, id := range ids {
		if _, err := f.pool.Exec(ctx,
			`UPDATE social_events SET turn_id = $1 WHERE id = $2`, turnID, id); err != nil {
			t.Fatalf("событие не привязано к ходу: %v", err)
		}
	}

	scope := memory.ChatScope(fmt.Sprintf("%d", chatID))
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, f.pool,
		memory.SourceJournalEvent, fmt.Sprintf("turn:%d", turnID), []string{scope})
	if err != nil {
		t.Fatalf("provenance эпизода не создан: %v", err)
	}
	source := memory.SourceRef{Kind: memory.SourceJournalEvent, Ref: fmt.Sprintf("turn:%d", turnID)}
	item, err := (memory.Items{}).Insert(ctx, f.pool, memory.NewItem{
		Kind:         "claim",
		Text:         "Коля собирает виниловые пластинки",
		Salience:     0.7,
		ProvenanceID: provenanceID,
		Source:       &source,
	}, scope)
	if err != nil {
		t.Fatalf("запись памяти эпизода не вставлена: %v", err)
	}

	entity, err := (memory.EntityRepo{}).FindOrCreate(ctx, f.pool, "person", "Коля", scope)
	if err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject')`, item.ID, entity.ID); err != nil {
		t.Fatalf("связь не создана: %v", err)
	}
	return ids, item.ID
}

// intentionStatus — текущий статус намерения.
func (f *fixture) intentionStatus(t *testing.T, ctx context.Context, id int64) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM intentions WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("намерение %d не читается: %v", id, err)
	}
	return status
}

// intentionNotBefore — срок следующей попытки (проверка переноса).
func (f *fixture) intentionNotBefore(t *testing.T, ctx context.Context, id int64) time.Time {
	t.Helper()
	var nb time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT not_before FROM intentions WHERE id = $1`, id).Scan(&nb); err != nil {
		t.Fatalf("намерение %d не читается: %v", id, err)
	}
	return nb
}

// walSteps — шаги WAL задачи (проверка следа §12.2).
func (f *fixture) walSteps(t *testing.T, ctx context.Context, unitID string) []runtime.StepRecord {
	t.Helper()
	steps, err := f.wal.Steps(ctx, unitID)
	if err != nil {
		t.Fatalf("WAL задачи %q не читается: %v", unitID, err)
	}
	return steps
}

// walDoneNames — имена исполненных шагов WAL.
func walDoneNames(steps []runtime.StepRecord) map[string]bool {
	out := map[string]bool{}
	for _, s := range steps {
		if s.Done {
			out[s.Step] = true
		}
	}
	return out
}

// seedDirectTurnPending — ход прямого обращения в очереди (created с событием):
// фон обязан откладываться (§5.2).
func (f *fixture) seedDirectTurnPending(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	eventID := f.seedPrivateChat(t, ctx, "срочный вопрос!", f.now)
	var turnID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO turns (chat_id, status) VALUES ($1, 'created') RETURNING id`, bgChatID).Scan(&turnID); err != nil {
		t.Fatalf("прямой ход не создан: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE social_events SET turn_id = $1 WHERE id = $2`, turnID, eventID); err != nil {
		t.Fatalf("событие не привязано к прямому ходу: %v", err)
	}
	return turnID
}

// counters для уникальности seed-строк.
var (
	updateIDCounter  int64 = 20_000_000
	messageIDCounter int64 = 20_000
)

func (f *fixture) nextUpdateID() int64  { updateIDCounter++; return updateIDCounter }
func (f *fixture) nextMessageID() int64 { messageIDCounter++; return messageIDCounter }

// newTestPool создаёт пул к свежей тестовой базе с полной схемой §18.
func newTestPool(t *testing.T) *pgxpool.Pool {
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

	dbName := fmt.Sprintf("dio_background_test_%d", time.Now().UnixNano())
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
	if result.Applied != 7 || result.Skipped != 0 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу 7/0", result.Applied, result.Skipped)
	}
	return pool
}

// swapDatabaseInDSN подменяет имя базы в URL (копия помощника 9.2–9.5, чтобы
// пакет background не зависел от внутренностей чужих тестов).
func swapDatabaseInDSN(dsn, dbName string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName), nil
}
