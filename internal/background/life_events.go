// События жизни (этап 24, решение владельца 2026-11-03, §8/§14/§15):
// у персонажа есть ВЫМЫШЛЕННАЯ жизнь, как биография persona_seed. Фон раз в
// 8–14 часов (случайный разброс через инжектируемый rand, не в тихих блоках,
// не больше 2 событий в сутки) просит модель придумать ОДНО бытовое событие.
// Модель предлагает текст, mood_delta с основанием и флаг «хочу рассказать»;
// runtime сам пишет experience в память, применяет настроение и — при желании
// рассказать — ставит share-намерение в последний личный чат, которое проходит
// обычную политику §14.3.
//
// Скоуп события — public: это жизнь самого персонажа, а не факт о собеседнике.
// Приватные записи чатов в контекст генерации не попадают вовсе, поэтому
// событие видно во всех чатах, но ничего личного не утекает (§9.2).
//
// Событие — отдельная фоновая задача с детерминированным id работы
// bg:life:<seq> (seq и срок следующего события — в runtime_state): WAL и
// чекпоинт переживают перезапуск, повторное применение идемпотентно по CAS
// расписания.
package background

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/luckystrker/ago/agent"
	agomiddleware "github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/response"
	agotask "github.com/luckystrker/ago/task"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/modebug"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

const (
	// LifeEventDelayMin/Max — пауза между событиями жизни: 8–14 часов
	// бодрствования со случайным разбросом. Крутится здесь: меньше — жизнь
	// гуще, больше — реже.
	LifeEventDelayMin = 8 * time.Hour
	LifeEventDelayMax = 14 * time.Hour
	// MaxLifeEventsPerDay — дневной потолок событий (те же пределы, что у
	// инициатив §14.3: два — уже достаточно «своей жизни»).
	MaxLifeEventsPerDay = 2

	// lifeEventTextLimit — событие в одну-две фразы: оно идёт в память и в
	// промпт следующих событий.
	lifeEventTextLimit = 400
	// lifeEventSalience — значимость experience: фон, не главный факт о мире.
	lifeEventSalience = 0.5
	// lifeEventShareDelayHours — задержка share «хочу рассказать»: как у
	// находки исследования §14.2, а не мгновенная отправка.
	lifeEventShareDelayHours = 0.3
	// lifeEventShareTopicLimit — тема share-намерения (dedupe §14.1).
	lifeEventShareTopicLimit = 80
	// lifeEventReasonLimit — основание mood_delta (как maxReasonLength хода).
	lifeEventReasonLimit = 500
	// lifeEventContextLimit — сколько последних событий видит модель, чтобы
	// не повторяться и держать континуитет.
	lifeEventContextLimit = 10

	// lifeEventSourcePrefix — префикс ссылки источника и источник истины о
	// том, что запись памяти — событие жизни.
	lifeEventSourcePrefix = "life_event:"
	// lifeScheduleKey — ключ расписания генератора в runtime_state (§18).
	lifeScheduleKey = "bg:life:schedule"
)

// lifeEventAddendum — правила фоновой генерации события жизни: модель только
// предлагает, ничего не отправляет; текст — от первого лица и бытовой.
const lifeEventAddendum = "Это фоновая задача: придумай ОДНО событие своей вымышленной жизни — " +
	"бытовое, конкретное, от первого лица, в одну-две фразы. Оно должно быть согласовано с биографией, " +
	"интересами и текущим занятием по расписанию. Не повторяй последние события и держи континуитет: " +
	"зарплата не приходит каждый день, а сгоревший блок питания не случается каждую смену. " +
	"Событие может быть мелким, смешным, раздражающим или приятным — как настоящий день. " +
	"mood_delta — как событие повлияло на настроение (valence −1..1, arousal и socialEnergy 0..1), " +
	"reason — короткое основание. want_to_tell — true, только если этим действительно хочется поделиться " +
	"с собеседником. Ничего не отправляй и не вызывай инструменты: верни только JSON LifeEventProposal."

// lifeSchedule — состояние генератора: номер следующей работы и срок события.
// Хранится в runtime_state: переживает перезапуск, не требует миграции.
type lifeSchedule struct {
	Seq  int64     `json:"seq"`
	Next time.Time `json:"next"`
}

// lifeEventPlan — работа, которую пора исполнить: детерминированный seq.
type lifeEventPlan struct {
	Seq int64
}

// lifeEventProposal — ответ модели (своя JSON-схема): одно событие, дельта
// настроения с основанием и необязательное желание рассказать.
type lifeEventProposal struct {
	Text       string               `json:"text"`
	MoodDelta  *character.MoodDelta `json:"mood_delta"`
	WantToTell bool                 `json:"want_to_tell,omitempty"`
}

// lifeEventRecord — прошлое событие для контекста генерации.
type lifeEventRecord struct {
	Text string
	At   time.Time
}

// lifeEventOutcome — итог исполнения для записи §20.
type lifeEventOutcome struct {
	Outcome  string
	Detail   string
	Text     string
	Shared   bool
	ChatID   int64
	Stats    modelStats
	Latency  time.Duration
	BudgetOp string
}

func lifeEventUnitID(seq int64) string { return fmt.Sprintf("bg:life:%d", seq) }

func lifeEventSourceRef(seq int64) string {
	return fmt.Sprintf("%s%d", lifeEventSourcePrefix, seq)
}

func lifeEventOperation(seq int64, attempt int) string {
	return fmt.Sprintf("model:bg:life:%d:%d", seq, attempt)
}

// PickLifeEventDelay — пауза до следующего события: равномерно в
// [LifeEventDelayMin, LifeEventDelayMax] с шагом минута. rnd nil — глобальный
// генератор (тесты/частичная сборка).
func PickLifeEventDelay(rnd *rand.Rand) time.Duration {
	minMinutes := int64(LifeEventDelayMin / time.Minute)
	maxMinutes := int64(LifeEventDelayMax / time.Minute)
	span := maxMinutes - minMinutes
	var n int64
	if rnd != nil {
		n = rnd.Int63n(span + 1)
	} else {
		n = rand.Int63n(span + 1)
	}
	return time.Duration(minMinutes+n) * time.Minute
}

// shiftOutOfQuiet переносит событие, попавшее в тихий блок, на его конец:
// ночью персонаж спит, а не проживает события.
func shiftOutOfQuiet(at time.Time, p character.Persona, loc *time.Location) time.Time {
	if until, quiet := p.QuietUntil(at, loc); quiet {
		return until
	}
	return at
}

// nextLifeEventAt — срок следующего события от now: случайная пауза, но не в
// тихом блоке расписания.
func (s *Scheduler) nextLifeEventAt(now time.Time) time.Time {
	return shiftOutOfQuiet(now.Add(PickLifeEventDelay(s.rnd)), s.persona, s.location)
}

// --- proposal-контракт модели ------------------------------------------------

// lifeEventJSONSchema — схема провода (strict json_schema): модель заполняет
// ровно эти поля, runtime проверяет границы.
func lifeEventJSONSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"text": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   lifeEventTextLimit,
				"description": "Одно бытовое событие твоей вымышленной жизни: 1–2 предложения от первого лица, конкретно.",
			},
			"mood_delta": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"valence": map[string]any{
						"type":        "number",
						"description": "Дельта валентности −0.3..0.3.",
					},
					"arousal": map[string]any{
						"type":        "number",
						"description": "Дельта энергии −0.3..0.3.",
					},
					"socialEnergy": map[string]any{
						"type":        "number",
						"description": "Дельта желания общаться −0.3..0.3.",
					},
					"reason": map[string]any{
						"type":        "string",
						"minLength":   1,
						"maxLength":   lifeEventReasonLimit,
						"description": "Короткое основание: почему событие так повлияло.",
					},
				},
				"required": []string{"reason"},
			},
			"want_to_tell": map[string]any{
				"type":        "boolean",
				"description": "true — событием действительно хочется поделиться с собеседником.",
			},
		},
		"required": []string{"text", "mood_delta"},
	}
}

// parseLifeEventProposal валидирует ответ модели: текст 1–400 рун, mood_delta
// с непустым основанием, лишние поля и хвост после JSON — ошибка (как у
// TurnResult, §11.2). Числа сжимаются до ходового лимита позже, при применении.
func parseLifeEventProposal(raw []byte) (lifeEventProposal, error) {
	raw = character.EscapeJSONControlChars(raw)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p lifeEventProposal
	if err := dec.Decode(&p); err != nil {
		return lifeEventProposal{}, fmt.Errorf("событие жизни: proposal: %w", err)
	}
	if dec.More() {
		return lifeEventProposal{}, errors.New("событие жизни: после JSON-объекта есть лишний текст")
	}

	p.Text = strings.TrimSpace(strings.Join(strings.Fields(p.Text), " "))
	if p.Text == "" {
		return lifeEventProposal{}, errors.New("событие жизни: пустой текст события")
	}
	if runes := []rune(p.Text); len(runes) > lifeEventTextLimit {
		return lifeEventProposal{}, fmt.Errorf("событие жизни: текст длиннее %d символов", lifeEventTextLimit)
	}
	if p.MoodDelta == nil {
		return lifeEventProposal{}, errors.New("событие жизни: нет mood_delta с основанием")
	}
	p.MoodDelta.Reason = strings.TrimSpace(p.MoodDelta.Reason)
	if p.MoodDelta.Reason == "" {
		return lifeEventProposal{}, errors.New("событие жизни: пустое основание mood_delta")
	}
	if runes := []rune(p.MoodDelta.Reason); len(runes) > lifeEventReasonLimit {
		return lifeEventProposal{}, fmt.Errorf("событие жизни: основание длиннее %d символов", lifeEventReasonLimit)
	}
	return p, nil
}

// lifeEventShareTopic — короткая тема share-намерения из самого события:
// первая фраза, одна строка, не длиннее потолка (dedupe §14.1).
func lifeEventShareTopic(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	if i := strings.IndexAny(text, ".!?;"); i > 0 {
		text = strings.TrimSpace(text[:i])
	}
	if runes := []rune(text); len(runes) > lifeEventShareTopicLimit {
		text = strings.TrimSpace(string(runes[:lifeEventShareTopicLimit]))
	}
	return text
}

// --- расписание генератора (runtime_state) -----------------------------------

// lifeScheduleNow — расписание из кэша такта; промах читает runtime_state.
// Кэш обновляют все записи расписания, поэтому на каждом такте база не нужна.
func (s *Scheduler) lifeScheduleNow(ctx context.Context) (*lifeSchedule, error) {
	if s.lifeCache != nil {
		return s.lifeCache, nil
	}
	sch, err := s.readLifeSchedule(ctx)
	if err != nil {
		return nil, err
	}
	s.lifeCache = sch
	return sch, nil
}

// readLifeSchedule читает состояние; nil — ещё не инициализировано.
func (s *Scheduler) readLifeSchedule(ctx context.Context) (*lifeSchedule, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM runtime_state WHERE key = $1`, lifeScheduleKey).Scan(&raw)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("событие жизни: расписание не читается: %w", err)
	}
	var sch lifeSchedule
	if err := json.Unmarshal(raw, &sch); err != nil {
		return nil, fmt.Errorf("событие жизни: расписание не разбирается: %w", err)
	}
	return &sch, nil
}

func (s *Scheduler) writeLifeSchedule(ctx context.Context, sch lifeSchedule) error {
	raw, err := json.Marshal(sch)
	if err != nil {
		return fmt.Errorf("событие жизни: расписание не сериализуется: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO runtime_state (key, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		lifeScheduleKey, string(raw)); err != nil {
		return fmt.Errorf("событие жизни: расписание не записано: %w", err)
	}
	s.lifeCache = &sch
	return nil
}

// initLifeSchedule заводит первое расписание: первое событие — не раньше чем
// через 8 часов, вне тихого блока.
func (s *Scheduler) initLifeSchedule(ctx context.Context) error {
	sch := lifeSchedule{Seq: 1, Next: s.nextLifeEventAt(s.now())}
	if err := s.writeLifeSchedule(ctx, sch); err != nil {
		return err
	}
	s.logInfo("событие жизни: следующее запланировано на %s", sch.Next.Format(time.RFC3339))
	return nil
}

// postponeLifeSchedule переносит событие той же работы (seq сохраняется):
// повторы и тихие часы не тратят событие и не путают WAL.
func (s *Scheduler) postponeLifeSchedule(ctx context.Context, seq int64, until time.Time) error {
	raw, err := json.Marshal(lifeSchedule{Seq: seq, Next: until})
	if err != nil {
		return fmt.Errorf("событие жизни: перенос не сериализуется: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runtime_state SET value = $2::jsonb, updated_at = now()
		WHERE key = $1 AND (value->>'seq')::bigint = $3`,
		lifeScheduleKey, string(raw), seq)
	if err != nil {
		return fmt.Errorf("событие жизни: перенос не записан: %w", err)
	}
	if tag.RowsAffected() > 0 {
		s.lifeCache = &lifeSchedule{Seq: seq, Next: until}
	} else {
		// Расписание уже ушло вперёд (или строки нет): кэш не достоверен.
		s.lifeCache = nil
	}
	return nil
}

// advanceLifeSchedule фиксирует исполненное/отменённое событие: seq + случайная
// пауза до следующего. CAS по seq — повторное применение после сбоя не
// сдвигает расписание дважды. advanced=false — событие уже учтено.
func (s *Scheduler) advanceLifeSchedule(ctx context.Context, seq int64) (bool, time.Time, error) {
	next := s.nextLifeEventAt(s.now())
	raw, err := json.Marshal(lifeSchedule{Seq: seq + 1, Next: next})
	if err != nil {
		return false, time.Time{}, fmt.Errorf("событие жизни: расписание не сериализуется: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runtime_state SET value = $2::jsonb, updated_at = now()
		WHERE key = $1 AND (value->>'seq')::bigint = $3`,
		lifeScheduleKey, string(raw), seq)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("событие жизни: расписание не сдвинуто: %w", err)
	}
	if tag.RowsAffected() > 0 {
		s.lifeCache = &lifeSchedule{Seq: seq + 1, Next: next}
	} else {
		// Расписание уже сдвинул другой прогон (или строки нет): кэш не
		// достоверен — следующий такт перечитает базу.
		s.lifeCache = nil
	}
	return tag.RowsAffected() > 0, next, nil
}

// --- контекст генерации: прошлые события и дневной счётчик -------------------

// lifeEventsToday считает события текущих суток в поясе персонажа.
func (s *Scheduler) lifeEventsToday(ctx context.Context) (int, error) {
	now := s.now()
	loc := s.location
	if loc == nil {
		loc = now.Location()
	}
	day := now.In(loc)
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	var n int
	// Считаем события, а не записи памяти: точный повтор текста дедуплицируется
	// памятью, но каждое событие имеет свой источник life_event:<seq>.
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM sources
		WHERE kind = $1 AND starts_with(ref, $2) AND created_at >= $3`,
		memory.SourceRuntimeAction, lifeEventSourcePrefix, dayStart).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("событие жизни: дневной счётчик не читается: %w", err)
	}
	return n, nil
}

// recentLifeEvents — последние события жизни (5–10) для континуитета.
func (s *Scheduler) recentLifeEvents(ctx context.Context, limit int) ([]lifeEventRecord, error) {
	if limit <= 0 {
		limit = lifeEventContextLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.text, max(src.created_at) AS at
		FROM memory_items m
		JOIN memory_sources ms ON ms.memory_id = m.id
		JOIN sources src ON src.id = ms.source_id
		WHERE src.kind = $1 AND starts_with(src.ref, $2) AND m.status = 'active'
		GROUP BY m.id, m.text
		ORDER BY max(src.created_at) DESC, m.text
		LIMIT $3`,
		memory.SourceRuntimeAction, lifeEventSourcePrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("событие жизни: прошлые события не читаются: %w", err)
	}
	defer rows.Close()
	var out []lifeEventRecord
	for rows.Next() {
		var ev lifeEventRecord
		if err := rows.Scan(&ev.Text, &ev.At); err != nil {
			return nil, fmt.Errorf("событие жизни: событие не читается: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("событие жизни: прошлые события не дочитаны: %w", err)
	}
	return out, nil
}

// --- такт фона ---------------------------------------------------------------

// maybeRunLifeEvent — триггер такта: инициализирует расписание, ждёт срока,
// переносит событие из тихого блока и исполняет ровно одну работу. Ошибка
// фоновой задачи не валит такт: событие жизни — необязательная активность.
func (s *Scheduler) maybeRunLifeEvent(ctx context.Context) error {
	sch, err := s.lifeScheduleNow(ctx)
	if err != nil {
		return err
	}
	if sch == nil {
		return s.initLifeSchedule(ctx)
	}
	now := s.now()
	if now.Before(sch.Next) {
		return nil
	}
	if until, quiet := s.persona.QuietUntil(now, s.location); quiet {
		s.logDebug("событие жизни: тихий блок, следующее — %s", until.Format(time.RFC3339))
		return s.postponeLifeSchedule(ctx, sch.Seq, until)
	}

	plan := lifeEventPlan{Seq: sch.Seq}
	out, runErr := s.runLifeEventGuarded(ctx, plan)
	if runErr != nil {
		out.Outcome, out.Detail = "error", runErr.Error()
		s.logError("событие жизни: генерация не удалась: %v", runErr)
		if perr := s.postponeLifeSchedule(ctx, plan.Seq, now.Add(s.opts.RetryBackoff)); perr != nil {
			s.logError("событие жизни: пауза повтора не записана: %v", perr)
		}
	}
	s.recordLifeEvent(plan, out)
	return nil
}

// runLifeEvent исполняет событие как ago-задачу: гейты (потолок, тишина,
// прямое обращение, бюджет, попытки) → генерация → применение одной
// транзакцией с CAS расписания (§12.2).
func (s *Scheduler) runLifeEvent(ctx context.Context, plan lifeEventPlan) (lifeEventOutcome, error) {
	started := s.now()
	unitID := lifeEventUnitID(plan.Seq)
	out := lifeEventOutcome{ChatID: 0}
	chatID, err := s.latestPrivateChat(ctx)
	if err != nil {
		return out, err
	}
	out.ChatID = chatID

	task, cp, err := s.startTask(ctx, unitID,
		fmt.Sprintf("Событие жизни #%d", plan.Seq),
		map[string]any{"seq": plan.Seq})
	if err != nil {
		return out, err
	}
	defer func() {
		if task.State() == agotask.StateRunning {
			_ = task.Fail(errors.New("исполнение вышло без терминального перехода"))
		}
	}()

	records, err := s.wal.Steps(ctx, unitID)
	if err != nil {
		return out, fmt.Errorf("фон: WAL задачи %q не читается: %w", unitID, err)
	}
	done := walDoneMap(records)

	var proposal lifeEventProposal
	if !done[stepGenerate] {
		// Дневной потолок: третье событие за сутки не генерируется, работа
		// закрывается, расписание уходит вперёд (иначе каждый такт проверял
		// бы одно и то же).
		events, cerr := s.lifeEventsToday(ctx)
		if cerr != nil {
			return out, cerr
		}
		if events >= MaxLifeEventsPerDay {
			reason := fmt.Sprintf("дневной потолок событий жизни (%d/сутки)", MaxLifeEventsPerDay)
			if _, _, aerr := s.advanceLifeSchedule(ctx, plan.Seq); aerr != nil {
				return out, aerr
			}
			_ = task.Cancel(reason)
			out.Outcome, out.Detail = "cancelled", reason
			out.Latency = s.now().Sub(started)
			return out, nil
		}

		// Тихий блок расписания: событие не проживается, срок — на конец
		// тишины (тот же seq, чтобы не потерять работу).
		if until, quiet := s.persona.QuietUntil(s.now(), s.location); quiet {
			if err := s.postponeLifeSchedule(ctx, plan.Seq, until); err != nil {
				return out, err
			}
			_ = task.Cancel("отложено: тихие часы расписания")
			out.Outcome = "deferred"
			out.Detail = "тихий блок расписания до " + until.Format("15:04")
			out.Latency = s.now().Sub(started)
			return out, nil
		}

		// §5.2: прямое обращение приоритетнее — событие подождёт следующий такт.
		pending, perr := s.directPending(ctx)
		if perr != nil {
			return out, perr
		}
		if pending {
			_ = task.Cancel("отложено: прямое обращение в очереди")
			out.Outcome, out.Detail = "deferred", "прямое обращение в очереди (§5.2)"
			out.Latency = s.now().Sub(started)
			return out, nil
		}

		// Потолок попыток (§16): вечный сбой провайдера не может повторяться
		// каждым тактом; событие закрывается, следующее — по расписанию.
		if cp.Attempt >= maxGenerateAttempts {
			reason := fmt.Sprintf("исчерпаны попытки генерации события жизни (%d)", maxGenerateAttempts)
			if _, _, aerr := s.advanceLifeSchedule(ctx, plan.Seq); aerr != nil {
				return out, aerr
			}
			_ = task.Fail(errors.New(reason))
			out.Outcome, out.Detail = "failed", reason
			out.Latency = s.now().Sub(started)
			return out, nil
		}

		// §16: дневной лимит пула фона — не сбой провайдера; ждём завтра.
		available, berr := s.budget.Available(ctx, runtime.BudgetPoolBackground, 1, s.dailyLimit)
		if berr != nil {
			return out, berr
		}
		if !available {
			if err := s.postponeLifeSchedule(ctx, plan.Seq, runtime.NextDay(s.now())); err != nil {
				return out, err
			}
			_ = task.Cancel("отложено: дневной лимит пула фона исчерпан")
			out.Outcome, out.Detail = "deferred", "бюджет фона исчерпан (§16)"
			out.Latency = s.now().Sub(started)
			return out, nil
		}

		cp.Attempt++
		if err := s.saveCheckpoint(ctx, unitID, cp); err != nil {
			return out, err
		}
		op := lifeEventOperation(plan.Seq, cp.Attempt)
		out.BudgetOp = op
		if err := s.wal.PlanStep(ctx, unitID, stepGenerate, "life_event_generate",
			map[string]any{"seq": plan.Seq, "attempt": cp.Attempt}); err != nil {
			return out, err
		}

		p, stats, gerr := s.generateLifeEvent(ctx, plan, op, chatID)
		out.Stats = stats
		if gerr != nil {
			if errors.Is(gerr, runtime.ErrBudgetExhausted) {
				cp.Attempt--
				if serr := s.saveCheckpoint(ctx, unitID, cp); serr != nil {
					return out, serr
				}
				if err := s.postponeLifeSchedule(ctx, plan.Seq, runtime.NextDay(s.now())); err != nil {
					return out, err
				}
				_ = task.Cancel("отложено: дневной лимит пула фона исчерпан")
				out.Outcome, out.Detail = "deferred", "бюджет фона исчерпан (§16)"
				out.Latency = s.now().Sub(started)
				return out, nil
			}
			var invalid *errInvalidResult
			if errors.As(gerr, &invalid) {
				// Невалидный proposal после repair: терминально, следующее
				// событие — по расписанию (повтор того же контекста бессмыслен).
				if _, _, aerr := s.advanceLifeSchedule(ctx, plan.Seq); aerr != nil {
					return out, aerr
				}
				_ = task.Fail(invalid)
				out.Outcome, out.Detail = "failed", invalid.Error()
				out.Latency = s.now().Sub(started)
				return out, nil
			}
			return out, fmt.Errorf("фон: генерация события жизни #%d не удалась: %w", plan.Seq, gerr)
		}

		raw, merr := json.Marshal(p)
		if merr != nil {
			return out, fmt.Errorf("фон: событие жизни не сериализуется: %w", merr)
		}
		if err := s.wal.StepDone(ctx, unitID, stepGenerate,
			map[string]any{"proposal": json.RawMessage(raw)}); err != nil {
			return out, err
		}
		proposal = p
	} else {
		raw, ok := walProposal(records, stepGenerate)
		if !ok {
			return out, fmt.Errorf("фон: событие жизни #%d: generate отмечен, но proposal в WAL нет", plan.Seq)
		}
		if proposal, err = parseLifeEventProposal(raw); err != nil {
			return out, fmt.Errorf("фон: сохранённое событие жизни #%d не валидно: %w", plan.Seq, err)
		}
	}
	out.Text = proposal.Text

	if !done[stepApply] {
		if err := s.wal.PlanStep(ctx, unitID, stepApply, "life_event_apply",
			map[string]any{"seq": plan.Seq}); err != nil {
			return out, err
		}
		applied, shared, detail, aerr := s.applyLifeEvent(ctx, plan, proposal, chatID)
		if aerr != nil {
			return out, aerr // транзакция откачена; WAL видит apply без done — повтор
		}
		if !applied {
			out.Outcome, out.Detail = "noop", "событие уже применено другим прогоном"
			out.Latency = s.now().Sub(started)
			return out, nil
		}
		out.Detail = detail
		out.Shared = shared
		if err := s.wal.StepDone(ctx, unitID, stepApply, map[string]any{"applied": true}); err != nil {
			return out, err
		}
		// Индексация эмбеддингов (16.8): событие сразу доступно и семантике.
		s.indexMemory(ctx)
	}

	_ = task.Complete(map[string]any{"seq": plan.Seq})
	out.Outcome = "completed"
	out.Latency = s.now().Sub(started)
	return out, nil
}

// runLifeEventGuarded — событие под recover: паника — ошибка этой задачи,
// остальные задачи такта и цикл фона продолжаются (16.2).
func (s *Scheduler) runLifeEventGuarded(ctx context.Context, plan lifeEventPlan) (out lifeEventOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("паника исполнения: %v", r)
			s.logError("паника события жизни #%d (задача изолирована, такт продолжается): %v", plan.Seq, r)
		}
	}()
	return s.runLifeEvent(ctx, plan)
}

// generateLifeEvent собирает контекст и делает модельный вызов с proposal-
// контрактом. Личные данные чатов в промпт не попадают: только персона
// (биография/интересы), текущее занятие и прошлые события жизни.
func (s *Scheduler) generateLifeEvent(ctx context.Context, plan lifeEventPlan, op string, chatID int64) (lifeEventProposal, modelStats, error) {
	system, err := s.lifeEventSystemPrompt()
	if err != nil {
		return lifeEventProposal{}, modelStats{}, err
	}
	userText, err := s.lifeEventUserPrompt(ctx)
	if err != nil {
		return lifeEventProposal{}, modelStats{}, err
	}
	return s.runLifeEventModel(ctx, op, system, userText, lifeEventSession(chatID))
}

func (s *Scheduler) lifeEventSystemPrompt() (string, error) {
	base, err := character.BuildSystemPrompt(s.persona, character.PromptExtras{}, s.prompt)
	if err != nil {
		return "", fmt.Errorf("событие жизни: промпт не собран: %w", err)
	}
	return base + "\n\n" + lifeEventAddendum, nil
}

func (s *Scheduler) lifeEventUserPrompt(ctx context.Context) (string, error) {
	now := s.now()
	loc := s.location
	// Панель состояния первой частью user-сообщения (28.4): время, «чем занят»
	// по расписанию и настроение. Собеседника у события жизни нет — личные
	// данные чатов в контекст не попадают.
	moodNow, _, err := character.NewMoodRepo().ReadDecayed(ctx, s.pool,
		s.persona.MoodTargetAt(now, loc))
	if err != nil {
		return "", fmt.Errorf("событие жизни: настроение не читается: %w", err)
	}
	panel := character.BackgroundPanel(now, loc, s.persona.ActivityAt(now, loc), "",
		character.DescribeMood(moodNow, s.persona.MoodBaseline), "")

	var b strings.Builder
	b.WriteString(character.ComposeUserText(panel, ""))
	b.WriteString("\n")

	events, err := s.recentLifeEvents(ctx, lifeEventContextLimit)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		b.WriteString("Последних событий жизни ещё нет — это первое.\n")
	} else {
		b.WriteString("Последние события твоей жизни (не повторяйся, держи континуитет):\n")
		for _, ev := range events {
			at := ev.At
			if loc != nil {
				at = at.In(loc)
			}
			fmt.Fprintf(&b, "- %s: %s\n", at.Format("02.01 15:04"), truncateRunes(ev.Text, 200))
		}
	}
	b.WriteString("Придумай одно новое событие и верни JSON LifeEventProposal.")
	return b.String(), nil
}

// lifeEventSession — сессия провайдера: по чату, а без чата — стабильная
// сессия персонажа (заголовок нужен провайдеру даже у внутренней задачи).
func lifeEventSession(chatID int64) string {
	if chatID == 0 {
		return "dio-life-events"
	}
	return sessionForChat(chatID)
}

// runLifeEventModel — модельный вызов события жизни: своя схема, один
// repair-retry (§11.3), пул бюджета background (§16), без инструментов.
func (s *Scheduler) runLifeEventModel(ctx context.Context, operation, system, userText, sessionID string) (lifeEventProposal, modelStats, error) {
	overrides := map[string]any(nil)
	if sessionID != "" {
		overrides = map[string]any{"session_id": sessionID}
	}
	cfg, err := s.model.Clone(overrides)
	if err != nil {
		return lifeEventProposal{}, modelStats{}, fmt.Errorf("фон: конфиг модели не клонирован: %w", err)
	}
	// Дамп модельных вызовов (40.1): событие жизни тоже видно в дампе.
	ctx = modebug.WithCall(ctx, modebug.NewCall("bg", dumpTask(operation)))

	fragments := []string{
		"Ограничения (runtime, не обсуждаемое):",
		"- Финальный ответ — только JSON по схеме LifeEventProposal, без текста вокруг него.",
		"- text — одно бытовое событие, 1–2 предложения от первого лица; не повторяй последние события.",
		"- mood_delta.reason — короткое основание; числа — дельты в пределах ±0.3.",
		"- want_to_tell — true только при настоящем желании поделиться; ничего не отправляй.",
	}

	counting := &usageCounter{}
	a := agent.New("dio-life-event", system,
		agent.WithConfig(cfg),
		agent.WithPromptFragments(fragments...),
		agent.WithResponseSchema(response.RawSchema(lifeEventJSONSchema(),
			response.WithRawName("LifeEventProposal"),
			response.WithRawDescription("Одно событие вымышленной жизни персонажа: текст, дельта настроения, желание рассказать."))),
		agent.WithMiddleware(
			s.cognitionMiddleware,
			func() agomiddleware.Middleware {
				return character.NewBudgetMiddlewarePool(s.budget, operation, s.dailyLimit,
					runtime.BudgetPoolBackground, s.logger)
			},
			func() agomiddleware.Middleware { return counting },
		),
	)

	first, err := a.Ask(ctx, userText)
	if err != nil {
		return lifeEventProposal{}, counting.stats(), err
	}
	body, cerr := first.Content()
	if cerr != nil {
		return lifeEventProposal{}, counting.stats(), cerr
	}
	p, perr := parseLifeEventProposal(body)
	if perr == nil {
		return p, counting.stats(), nil
	}

	// Один repair-retry (§11.3): тот же контекст, модели показана ошибка.
	fixed, ferr := first.Ask(ctx, character.RepairHint(perr, "LifeEventProposal"))
	if ferr != nil {
		return lifeEventProposal{}, counting.stats(), ferr
	}
	fbody, cerr := fixed.Content()
	if cerr != nil {
		return lifeEventProposal{}, counting.stats(), cerr
	}
	fp, fperr := parseLifeEventProposal(fbody)
	if fperr != nil {
		return lifeEventProposal{}, counting.stats(), &errInvalidResult{err: fperr}
	}
	return fp, counting.stats(), nil
}

// --- применение --------------------------------------------------------------

// applyLifeEvent применяет событие одной транзакцией: CAS расписания (ровно
// один раз), experience в public-скоуп, mood_delta и — при желании рассказать
// — share-намерение в личный чат. Источник и разрешение назначает runtime
// (§9.1, §10.1): событие видно во всех чатах, потому что это жизнь персонажа,
// а не факт о собеседнике.
func (s *Scheduler) applyLifeEvent(ctx context.Context, plan lifeEventPlan, p lifeEventProposal, chatID int64) (bool, bool, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, "", fmt.Errorf("событие жизни: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// CAS расписания: если seq уже сдвинут, событие применено прошлым прогоном.
	next := s.nextLifeEventAt(s.now())
	rawNext, err := json.Marshal(lifeSchedule{Seq: plan.Seq + 1, Next: next})
	if err != nil {
		return false, false, "", fmt.Errorf("событие жизни: расписание не сериализуется: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE runtime_state SET value = $2::jsonb, updated_at = now()
		WHERE key = $1 AND (value->>'seq')::bigint = $3`,
		lifeScheduleKey, string(rawNext), plan.Seq)
	if err != nil {
		return false, false, "", fmt.Errorf("событие жизни: CAS расписания: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, false, "", nil // уже применено; транзакция откатится
	}

	ref := lifeEventSourceRef(plan.Seq)
	source := memory.SourceRef{Kind: memory.SourceRuntimeAction, Ref: ref}
	sourceID, err := (memory.SourceRepo{}).EnsureSource(ctx, tx, source)
	if err != nil {
		return false, false, "", fmt.Errorf("событие жизни: источник не создан: %w", err)
	}
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, tx,
		memory.SourceRuntimeAction, ref, []string{memory.ScopePublic})
	if err != nil {
		return false, false, "", fmt.Errorf("событие жизни: разрешение не создано: %w", err)
	}
	occurred := s.now()
	item, err := (memory.Items{}).Insert(ctx, tx, memory.NewItem{
		Kind:         memory.KindExperience,
		Text:         p.Text,
		Salience:     lifeEventSalience,
		OccurredAt:   &occurred,
		ProvenanceID: provenanceID,
		Source:       &source,
	}, memory.ScopePublic)
	if err != nil {
		return false, false, "", fmt.Errorf("событие жизни: запись памяти: %w", err)
	}
	// Источник события привязывается и к уже существующей записи: точный
	// повтор текста дедуплицируется памятью (18.8), но событие всё равно
	// остаётся отдельным в sources — по нему считаются сутки и континуитет.
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_sources (memory_id, source_id)
		SELECT $1, $2 WHERE NOT EXISTS (
		  SELECT 1 FROM memory_sources WHERE memory_id = $1 AND source_id = $2)`,
		item.ID, sourceID); err != nil {
		return false, false, "", fmt.Errorf("событие жизни: источник не привязан к записи: %w", err)
	}

	if p.MoodDelta != nil && moodDeltaSet(*p.MoodDelta) {
		if _, err := character.NewMoodRepo().ApplyDelta(ctx, tx,
			s.persona.MoodTargetAt(s.now(), s.location),
			character.ClampMoodDelta(*p.MoodDelta)); err != nil {
			return false, false, "", fmt.Errorf("событие жизни: дельта настроения: %w", err)
		}
	}

	shared := false
	if p.WantToTell {
		if chatID == 0 {
			s.logDebug("событие жизни: личного чата нет — share пропущен")
		} else {
			_, created, cerr := character.CreateIntentionTx(ctx, tx, character.IntentionParams{
				Kind:       KindShare,
				ChatID:     chatID,
				ScopeID:    memory.ChatScope(fmt.Sprintf("%d", chatID)),
				Topic:      lifeEventShareTopic(p.Text),
				Reason:     "событие жизни: " + truncateRunes(p.Text, 400),
				DelayHours: lifeEventShareDelayHours,
			})
			if cerr != nil {
				return false, false, "", fmt.Errorf("событие жизни: share-намерение: %w", cerr)
			}
			shared = created
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, false, "", fmt.Errorf("событие жизни: транзакция не зафиксирована: %w", err)
	}
	s.lifeCache = &lifeSchedule{Seq: plan.Seq + 1, Next: next}
	detail := fmt.Sprintf("память=%d", item.ID)
	return true, shared, detail, nil
}

// moodDeltaSet — есть ли в дельте хоть одно число: пустая дельта не трогает
// updated_at настроения.
func moodDeltaSet(d character.MoodDelta) bool {
	return d.Valence != nil || d.Arousal != nil || d.SocialEnergy != nil
}

// recordLifeEvent пишет запись фоновой задачи §20 (с текстом события и
// решением share) и отдельную строку лога «событие жизни: …».
func (s *Scheduler) recordLifeEvent(plan lifeEventPlan, out lifeEventOutcome) {
	s.writeRecord(TaskRecord{
		Unit:     lifeEventUnitID(plan.Seq),
		Kind:     "life_event",
		ChatID:   out.ChatID,
		Decision: "life_event",
		Outcome:  out.Outcome,
		Latency:  out.Latency,
		Stats:    out.Stats,
		BudgetOp: out.BudgetOp,
		Detail:   fmt.Sprintf("text=%q share=%v %s", truncateRunes(out.Text, 200), out.Shared, out.Detail),
	})
	if out.Text != "" {
		s.logInfo("событие жизни: %s", truncateRunes(out.Text, 200))
	}
}
