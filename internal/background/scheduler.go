// Планировщик фона (§14.2): внутрипроцессный таймер проверяет due-работу
// каждые 15–30 секунд с jitter. Проверка НЕ вызывает модель — она читает
// таблицы и запускает исполнение готовых задач (executor.go). Один
// планировщик обслуживает намерения, summaries и cleanup (§14.2); при
// прямом обращении фон не запускается вовсе (§5.2).
package background

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/internal/web"
)

// Границы такта §14.2 («ориентировочно каждые 15–30 секунд с jitter»).
const (
	DefaultMinInterval = 15 * time.Second
	DefaultMaxInterval = 30 * time.Second
	// RetryBackoff — пауза перед повтором после транзитного сбоя задачи
	// (§12.1: retry_wait, растущий с номером попытки; для фона — плоская пауза).
	DefaultRetryBackoff = 15 * time.Minute
)

// Options — параметры такта (контролируются тестами; нули — значения по умолчанию).
type Options struct {
	MinInterval      time.Duration
	MaxInterval      time.Duration
	ClaimLimit       int // намерений за такт
	ConsolidateLimit int // эпизодов за такт
	RetryBackoff     time.Duration
}

func (o Options) withDefaults() Options {
	if o.MinInterval <= 0 {
		o.MinInterval = DefaultMinInterval
	}
	if o.MaxInterval < o.MinInterval {
		o.MaxInterval = DefaultMaxInterval
	}
	if o.ClaimLimit <= 0 {
		o.ClaimLimit = 3
	}
	if o.ConsolidateLimit <= 0 {
		o.ConsolidateLimit = 2
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = DefaultRetryBackoff
	}
	return o
}

// Deps — зависимости планировщика.
type Deps struct {
	Pool            *pgxpool.Pool
	Journal         *character.TurnJournal     // журнал ходов (§12.1), включая ходы инициатив
	WAL             *runtime.TaskWAL           // WAL фоновых задач (event_streams, §12.2)
	Checkpoints     *runtime.PgCheckpointStore // чекпоинты задач (task_checkpoints)
	Budget          *runtime.BudgetStore       // резерв бюджета (§16, пул background)
	Memory          *memory.Toolset            // инструменты памяти (§10.4); nil — без инструментов
	Web             *web.Toolset               // веб-инструменты exploration (§15); nil или выключен — без веба
	WebMaxSearch    int                        // бюджет поисков одной explore-задачи (§15/§16); 0 — дефолт
	WebMaxFetch     int                        // бюджет загрузок одной explore-задачи; 0 — дефолт
	Persona         character.Persona          // активная персона (§7.1)
	PromptTemplate  string                     // каркас системного промпта (§7.1); пусто — встроенный
	Model           *character.ChatConfig      // nil — модель не настроена: фон не мыслит
	DailyCallsLimit int64                      // дневной лимит пула background (§16); 0 — не ограничивать
	ToolCallsLimit  int                        // лимит обращений к инструментам за ход (§11.2)
	Logger          *log.Logger
	Now             func() time.Time // часы; nil — time.Now (тесты подменяют)
	Rand            *rand.Rand       // источник jitter; nil — локальный
}

// Scheduler — фон: намерения, консолидация, cleanup, наблюдаемость (§14, §20).
// Модельные задачи исполняются последовательно в такте: один когнитивный
// worker (§5.2) — планировщик не создаёт параллельной когниции.
type Scheduler struct {
	pool         *pgxpool.Pool
	journal      *character.TurnJournal
	wal          *runtime.TaskWAL
	checkpoints  *runtime.PgCheckpointStore
	budget       *runtime.BudgetStore
	memory       *memory.Toolset
	web          *web.Toolset
	webMaxSearch int
	webMaxFetch  int
	persona      character.Persona
	prompt       string
	model        *character.ChatConfig
	dailyLimit   int64
	toolLimit    int
	logger       *log.Logger
	now          func() time.Time
	rnd          *rand.Rand
	intentions   *IntentionRepo
	opts         Options

	// Наблюдаемость роста очереди (§20): между тактами.
	mu           sync.Mutex
	lastPending  int
	growthStreak int
	growthWarned bool
}

// New строит планировщик.
func New(deps Deps, opts Options) (*Scheduler, error) {
	if deps.Pool == nil {
		return nil, errDeps("пул БД")
	}
	if deps.WAL == nil {
		return nil, errDeps("WAL задач")
	}
	if deps.Checkpoints == nil {
		return nil, errDeps("хранилище чекпоинтов")
	}
	if deps.Budget == nil {
		return nil, errDeps("бюджет")
	}
	logger := deps.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	rnd := deps.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Scheduler{
		pool:         deps.Pool,
		journal:      deps.Journal,
		wal:          deps.WAL,
		checkpoints:  deps.Checkpoints,
		budget:       deps.Budget,
		memory:       deps.Memory,
		web:          deps.Web,
		webMaxSearch: deps.WebMaxSearch,
		webMaxFetch:  deps.WebMaxFetch,
		persona:      deps.Persona,
		prompt:       deps.PromptTemplate,
		model:        deps.Model,
		dailyLimit:   deps.DailyCallsLimit,
		toolLimit:    deps.ToolCallsLimit,
		logger:       logger,
		now:          now,
		rnd:          rnd,
		intentions:   NewIntentionRepo(deps.Pool),
		opts:         opts.withDefaults(),
	}, nil
}

type depsError string

func (e depsError) Error() string {
	return "фон: не задана зависимость: " + string(e)
}

func errDeps(what string) error { return depsError(what) }

// nextDelay — пауза до следующего такта: равномерный jitter на [min, max]
// (§14.2: «каждые 15–30 секунд с jitter»).
func (s *Scheduler) nextDelay() time.Duration {
	span := s.opts.MaxInterval - s.opts.MinInterval
	return s.opts.MinInterval + time.Duration(s.rnd.Int63n(int64(span)+1))
}

// Run запускает цикл планировщика: восстановление (§12.2) и такты с jitter.
// Выход — по отмене контекста (shutdown §5.3).
func (s *Scheduler) Run(ctx context.Context) error {
	// Восстановление: running-намерения прошлого процесса → pending; их задачи
	// продолжатся с чекпоинта/WAL при следующем claim (§12.2, singleton §5.1).
	n, err := s.intentions.ResetRunning(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.logInfo("фон: восстановление: %d незавершённых намерений возвращены в pending (продолжение с чекпоинта/WAL)", n)
	}
	// Модель может быть не настроена (поддерживаемое состояние: Tick не мыслит);
	// значения map в Go вычисляются жадно — прямая запись в map паниковала бы
	// на nil-модели (дефект 9.6, вскрыт сборкой приложения 9.7).
	modelName := "не настроена"
	if s.model != nil {
		modelName = s.model.Model()
	}
	s.logInfo("планировщик фона запущен: такт %v..%v, модель %s",
		s.opts.MinInterval, s.opts.MaxInterval, modelName)

	for {
		delay := s.nextDelay()
		select {
		case <-ctx.Done():
			s.logInfo("планировщик фона остановлен")
			return nil
		case <-time.After(delay):
			if err := s.Tick(ctx); err != nil {
				s.logError("такт планировщика фона не удался: %v", err)
			}
		}
	}
}

// Tick — один такт: наблюдаемость (§20), cleanup, отбор и исполнение работы.
// Проверка не вызывает модель (§14.2); модель вызывают только исполнители
// готовых задач. Экспортирован для тестов (в TS — tick).
func (s *Scheduler) Tick(ctx context.Context) error {
	stats, err := s.QueueStats(ctx)
	if err != nil {
		return err
	}
	s.observeTick(stats)

	// Cleanup (§14.2): просроченные pending-намерения — терминальная отмена;
	// это не модельная работа, делается и при занятой очереди.
	if expired, err := s.intentions.ExpireOverdue(ctx); err != nil {
		return err
	} else if expired > 0 {
		s.logInfo("фон: отменено просроченных намерений: %d (§14.1 expiresAt)", expired)
	}

	// §5.2: при прямом обращении фон не запускается — следующий фоновый шаг
	// откладывается; ingest и доставка продолжают свою работу.
	if stats.PendingTurns > 0 {
		s.logInfo("фон отложен: в очереди прямое обращение (§5.2): ходов=%d", stats.PendingTurns)
		return nil
	}

	// Без модели фон не мыслит: ни намерения, ни консолидация.
	if s.model == nil {
		return nil
	}

	claimed, err := s.intentions.ClaimDue(ctx, s.opts.ClaimLimit)
	if err != nil {
		return err
	}
	for _, it := range claimed {
		out, rerr := s.runIntention(ctx, it)
		if rerr != nil {
			out.Outcome = "error"
			out.Detail = rerr.Error()
			s.deferIntention(ctx, it, rerr)
		}
		s.recordIntention(it, out)
	}

	episodes, err := s.dueEpisodes(ctx, s.opts.ConsolidateLimit)
	if err != nil {
		return err
	}
	for _, ep := range episodes {
		cooled, cerr := s.consolidationCooled(ctx, ep)
		if cerr != nil {
			return cerr
		}
		if cooled {
			continue
		}
		out, rerr := s.runConsolidation(ctx, ep)
		if rerr != nil {
			out.Outcome = "error"
			out.Detail = rerr.Error()
			// Эпизод не помечен — события не summarized; повтор на следующем такте.
			s.logError("консолидация эпизода чата %d (%d событий) не удалась: %v", ep.ChatID, len(ep.EventIDs), rerr)
		}
		s.recordConsolidation(ep, out)
	}
	return nil
}

// deferIntention — реакция на транзитный сбой задачи: возврат в pending с
// паузой (§12.1 retry_wait). Исчерпание бюджета фона откладывает работу на
// завтра: повторять в тот же день бессмысленно (§16).
func (s *Scheduler) deferIntention(ctx context.Context, it Intention, runErr error) {
	notBefore := s.now().Add(s.opts.RetryBackoff)
	if errors.Is(runErr, runtime.ErrBudgetExhausted) {
		notBefore = nextDay(s.now())
		s.logInfo("намерение %d отложено на завтра: бюджет фона исчерпан (§16)", it.ID)
	}
	if err := s.intentions.Release(ctx, it.ID, notBefore); err != nil {
		s.logError("намерение %d не возвращено в pending после сбоя: %v", it.ID, err)
	}
}

// nextDay — момент после полуночи UTC (00:05): повтор фонового пула с нового дня.
func nextDay(now time.Time) time.Time {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	return midnight.Add(5 * time.Minute)
}

// dueEpisodes — готовые к консолидации эпизоды (§10.2). Проверка читает
// таблицы и не вызывает модель (§14.2).
func (s *Scheduler) dueEpisodes(ctx context.Context, limit int) ([]Episode, error) {
	return pendingEpisodes(ctx, s.pool, s.now(), limit)
}

// directPending — есть ли ходы прямых обращений в очереди (created с
// привязанными событиями). Ходы инициатив событий не имеют и в счёт не идут:
// собственный ход задачи не блокирует её же продолжение (иначе сбой между
// созданием хода и применением заблокировал бы фон навсегда).
func (s *Scheduler) directPending(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM turns t
		  WHERE t.status = 'created'
		    AND EXISTS (SELECT 1 FROM social_events e WHERE e.turn_id = t.id)
		)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("фон: очередь прямых обращений не читается: %w", err)
	}
	return exists, nil
}
