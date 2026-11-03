//go:build db

// БД-тесты событий жизни (этап 24, гейт этапа): фон генерирует одно событие
// вымышленной жизни, пишет experience в публичный скоуп персоны (виден во всех
// чатах, но без приватных данных), применяет mood_delta и при «хочу рассказать»
// ставит share-намерение в последний личный чат. Тихие часы и дневной потолок
// останавливают генератор; свежее событие достаётся карточкой по релевантному
// сообщению через обычный фильтр приватности §9.2.
package background

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
)

// lifeEventDemoJSON — бытовое событие с основанием настроения и желанием
// рассказать. Текст содержит «зарплата»: по нему карточка находит событие.
const lifeEventDemoJSON = `{"text":"сегодня пришла зарплата, наконец-то закрыл долг за интернет",` +
	`"mood_delta":{"valence":0.2,"arousal":0.1,"reason":"стало легче на душе"},` +
	`"want_to_tell":true}`

// seedLifeSchedule пишет расписание генератора (runtime_state, §18).
func (f *fixture) seedLifeSchedule(t *testing.T, ctx context.Context, seq int64, next time.Time) {
	t.Helper()
	raw, err := json.Marshal(lifeSchedule{Seq: seq, Next: next})
	if err != nil {
		t.Fatalf("расписание событий не сериализуется: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO runtime_state (key, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		lifeScheduleKey, string(raw)); err != nil {
		t.Fatalf("расписание событий не записано: %v", err)
	}
}

// lifeScheduleOf читает расписание генератора.
func lifeScheduleOf(t *testing.T, ctx context.Context, f *fixture) lifeSchedule {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT value FROM runtime_state WHERE key = $1`, lifeScheduleKey).Scan(&raw); err != nil {
		t.Fatalf("расписание событий не читается: %v", err)
	}
	var sch lifeSchedule
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatalf("расписание событий не разбирается: %v", err)
	}
	return sch
}

// seedLifeMemory вставляет готовое событие жизни: experience в public-скоупе
// с источником runtime_action life_event:<seq>.
func (f *fixture) seedLifeMemory(t *testing.T, ctx context.Context, seq int64, text string, at time.Time) int64 {
	t.Helper()
	ref := lifeEventSourceRef(seq)
	source := memory.SourceRef{Kind: memory.SourceRuntimeAction, Ref: ref}
	if _, err := (memory.SourceRepo{}).EnsureSource(ctx, f.pool, source); err != nil {
		t.Fatalf("источник события не создан: %v", err)
	}
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, f.pool,
		memory.SourceRuntimeAction, ref, []string{memory.ScopePublic})
	if err != nil {
		t.Fatalf("разрешение события не создано: %v", err)
	}
	item, err := (memory.Items{}).Insert(ctx, f.pool, memory.NewItem{
		Kind:         memory.KindExperience,
		Text:         text,
		Salience:     lifeEventSalience,
		OccurredAt:   &at,
		ProvenanceID: provenanceID,
		Source:       &source,
	}, memory.ScopePublic)
	if err != nil {
		t.Fatalf("событие в память не записано: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE memory_items SET recorded_at = $2 WHERE id = $1`, item.ID, at); err != nil {
		t.Fatalf("время записи события не сдвинуто: %v", err)
	}
	return item.ID
}

// TestLifeEventGeneratedAndSharedDB — главный сценарий 24.1–24.2: такт фона
// генерирует событие, пишет experience в публичный скоуп, применяет
// настроение и ставит share в последний личный чат (без прямой отправки).
func TestLifeEventGeneratedAndSharedDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.enableLifeEvents()
	f.sched.rnd = rand.New(rand.NewSource(7))
	model := f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: lifeEventDemoJSON}},
	)
	f.seedPrivateChat(t, ctx, "привет", f.now.Add(-2*time.Hour))
	f.seedLifeSchedule(t, ctx, 1, f.now.Add(-time.Minute))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт с событием жизни: %v", err)
	}

	// 1. Experience в публичном скоупе персоны: не в чате собеседника, но
	// виден во всех чатах (public + provenance public).
	var (
		text    string
		scopeID string
		kind    string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT m.text, m.scope_id, m.kind
		FROM memory_items m
		JOIN memory_sources ms ON ms.memory_id = m.id
		JOIN sources s ON s.id = ms.source_id
		WHERE s.kind = $1 AND s.ref = $2`,
		memory.SourceRuntimeAction, lifeEventSourceRef(1)).Scan(&text, &scopeID, &kind); err != nil {
		t.Fatalf("событие жизни не записано в память: %v", err)
	}
	if scopeID != memory.ScopePublic {
		t.Fatalf("скоуп события = %q, хочу %q (событие видно во всех чатах персоны)",
			scopeID, memory.ScopePublic)
	}
	if kind != memory.KindExperience {
		t.Fatalf("вид записи события = %q, хочу experience", kind)
	}
	if !strings.Contains(text, "зарплата") {
		t.Fatalf("текст события потерян: %q", text)
	}

	// 2. mood_delta применена: валентность выросла относительно базовой линии.
	var valence float64
	if err := f.pool.QueryRow(ctx,
		`SELECT valence FROM character_state WHERE id = 1`).Scan(&valence); err != nil {
		t.Fatalf("настроение не читается: %v", err)
	}
	if valence <= 0.2 {
		t.Fatalf("mood_delta события не применена: valence=%.3f", valence)
	}

	// 3. «Хочу рассказать» стало share-намерением в последний личный чат,
	// прямой отправки нет — политику §14.3 share проходит сам.
	var (
		shareChat   int64
		shareStatus string
		shareReason string
		shareTopic  string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT coalesce(target_chat_id, 0), status, reason, topic
		FROM intentions WHERE kind = $1 ORDER BY id DESC LIMIT 1`,
		KindShare).Scan(&shareChat, &shareStatus, &shareReason, &shareTopic); err != nil {
		t.Fatalf("share-намерение события не создано: %v", err)
	}
	if shareChat != bgChatID {
		t.Fatalf("share ушёл в чат %d, хочу последний личный %d", shareChat, bgChatID)
	}
	if shareStatus != "pending" {
		t.Fatalf("share должен ждать своей политики, статус %s", shareStatus)
	}
	if !strings.Contains(shareReason, "зарплата") {
		t.Fatalf("в причине share нет события: %q", shareReason)
	}
	if !strings.Contains(strings.ToLower(shareTopic), "зарплата") {
		t.Fatalf("тема share не отражает событие: %q", shareTopic)
	}
	var deliveries int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE chat_id = $1`, bgChatID).Scan(&deliveries); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if deliveries != 0 {
		t.Fatalf("событие жизни отправило сообщение напрямую: %d доставок", deliveries)
	}

	// 4. Расписание сдвинуто: следующее событие не раньше чем через 8 часов,
	// и это уже другая работа (seq+1) — WAL/чекпоинт не переиспользуются.
	sch := lifeScheduleOf(t, ctx, f)
	if sch.Seq != 2 {
		t.Fatalf("seq расписания = %d, хочу 2", sch.Seq)
	}
	if sch.Next.Before(f.now.Add(LifeEventDelayMin)) {
		t.Fatalf("следующее событие %v раньше минимальной паузы %v", sch.Next, LifeEventDelayMin)
	}
	if calls := len(model.Calls()); calls != 1 {
		t.Fatalf("модельных вызовов %d, хочу 1", calls)
	}
}

// TestLifeEventResumeFromWALDB — 24.1: сбой между генерацией и применением
// (или перезапуск процесса) продолжает событие с WAL без нового модельного
// вызова: расписание сдвигается только при применении, поэтому seq тот же.
func TestLifeEventResumeFromWALDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.enableLifeEvents()
	model := f.withModel(t) // ответов нет: лишний вызов модели уронит тест
	f.seedPrivateChat(t, ctx, "привет", f.now.Add(-2*time.Hour))
	f.seedLifeSchedule(t, ctx, 1, f.now.Add(-time.Minute))

	unitID := lifeEventUnitID(1)
	if err := f.wal.PlanStep(ctx, unitID, stepGenerate, "life_event_generate",
		map[string]any{"seq": int64(1), "attempt": 1}); err != nil {
		t.Fatalf("WAL generate не запланирован: %v", err)
	}
	if err := f.wal.StepDone(ctx, unitID, stepGenerate,
		map[string]any{"proposal": json.RawMessage(lifeEventDemoJSON)}); err != nil {
		t.Fatalf("WAL generate не закрыт: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт возобновления события: %v", err)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("возобновление сгенерировало событие заново: %d вызовов модели", calls)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items m
		JOIN memory_sources ms ON ms.memory_id = m.id
		JOIN sources s ON s.id = ms.source_id
		WHERE s.kind = $1 AND s.ref = $2`,
		memory.SourceRuntimeAction, lifeEventSourceRef(1)).Scan(&n); err != nil {
		t.Fatalf("событие не читается: %v", err)
	}
	if n != 1 {
		t.Fatalf("событие из WAL не применено: %d записей", n)
	}
	if sch := lifeScheduleOf(t, ctx, f); sch.Seq != 2 {
		t.Fatalf("расписание не сдвинуто после возобновления: seq=%d", sch.Seq)
	}
}

// TestLifeEventDailyCountUsesSourcesDB — дневной потолок считает события, а не
// записи памяти: точный повтор текста дедуплицируется памятью (18.8), но
// каждое событие имеет свой источник life_event:<seq>.
func TestLifeEventDailyCountUsesSourcesDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.enableLifeEvents()
	f.seedLifeMemory(t, ctx, 1, "сосед опять сверлит", f.now.Add(-2*time.Hour))
	f.seedLifeMemory(t, ctx, 2, "сосед опять сверлит", f.now.Add(-time.Hour))

	n, err := f.sched.lifeEventsToday(ctx)
	if err != nil {
		t.Fatalf("дневной счётчик не прочитан: %v", err)
	}
	if n != 2 {
		t.Fatalf("событий за сутки %d, хочу 2 (источники, а не записи памяти)", n)
	}
	var items int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE scope_id = $1 AND text = $2`,
		memory.ScopePublic, "сосед опять сверлит").Scan(&items); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if items != 1 {
		t.Fatalf("повтор текста задублировал память: %d записей", items)
	}
}

// TestLifeEventQuietHoursNoEventDB — ночью событие не проживается: генератор
// переносит срок на конец тихого блока и модель не зовёт.
func TestLifeEventQuietHoursNoEventDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.enableLifeEvents()
	loc := moscowLoc(t)
	sched := quietSchedule()
	f.sched.persona.Schedule = &sched
	f.sched.location = loc
	night := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	f.setNow(night)
	model := f.withModel(t)
	f.seedPrivateChat(t, ctx, "спокойной ночи", night.Add(-2*time.Hour))
	f.seedLifeSchedule(t, ctx, 1, night.Add(-time.Minute))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("ночной такт: %v", err)
	}
	var events int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items m
		JOIN memory_sources ms ON ms.memory_id = m.id
		JOIN sources s ON s.id = ms.source_id
		WHERE s.kind = $1 AND s.ref LIKE 'life_event:%'`,
		memory.SourceRuntimeAction).Scan(&events); err != nil {
		t.Fatalf("события жизни не читаются: %v", err)
	}
	if events != 0 {
		t.Fatalf("в тихий блок прожито событие: %d", events)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("в тихий блок модель вызвана %d раз", calls)
	}
	sch := lifeScheduleOf(t, ctx, f)
	if sch.Seq != 1 {
		t.Fatalf("тихий блок потратил seq: %d, хочу 1", sch.Seq)
	}
	want := time.Date(2026, 11, 3, 8, 0, 0, 0, loc)
	if !sch.Next.Equal(want) {
		t.Fatalf("срок события после тишины = %v, хочу %v", sch.Next, want)
	}
}

// TestLifeEventDailyCapDB — дневной потолок 2 события в сутки: третье не
// генерируется, модельный вызов не тратится, расписание уходит вперёд.
func TestLifeEventDailyCapDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.enableLifeEvents()
	model := f.withModel(t)
	f.seedPrivateChat(t, ctx, "привет", f.now.Add(-2*time.Hour))
	f.seedLifeMemory(t, ctx, 100, "утром сосед опять сверлил стену", f.now.Add(-5*time.Hour))
	f.seedLifeMemory(t, ctx, 101, "вышел новый вкус чипсов, попробовал", f.now.Add(-3*time.Hour))
	f.seedLifeSchedule(t, ctx, 1, f.now.Add(-time.Minute))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт с исчерпанным потолком: %v", err)
	}
	var events int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items m
		JOIN memory_sources ms ON ms.memory_id = m.id
		JOIN sources s ON s.id = ms.source_id
		WHERE s.kind = $1 AND s.ref LIKE 'life_event:%'`,
		memory.SourceRuntimeAction).Scan(&events); err != nil {
		t.Fatalf("события жизни не читаются: %v", err)
	}
	if events != MaxLifeEventsPerDay {
		t.Fatalf("сверх дневного потолка создано событие: %d, хочу %d", events, MaxLifeEventsPerDay)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("модель вызвана %d раз при исчерпанном потолке", calls)
	}
	if sch := lifeScheduleOf(t, ctx, f); sch.Seq != 2 {
		t.Fatalf("seq расписания после потолка = %d, хочу 2", sch.Seq)
	}
}

// TestLifeEventReachableFromCardDB — 24.2: свежее событие жизни доступно в
// разговорном ходе через карточку памяти по релевантному сообщению. Событие
// лежит в public-скоупе, а карточка собрана для аудитории чата: фильтр §9.2
// пропускает его, не смешивая с приватными записями.
func TestLifeEventReachableFromCardDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.enableLifeEvents()
	f.seedLifeSchedule(t, ctx, 1, f.now)
	p := lifeEventProposal{
		Text:       "сегодня пришла зарплата, наконец-то закрыл долг за интернет",
		MoodDelta:  &character.MoodDelta{Reason: "стало легче"},
		WantToTell: false,
	}
	applied, _, detail, err := f.sched.applyLifeEvent(ctx, lifeEventPlan{Seq: 1}, p, bgChatID)
	if err != nil {
		t.Fatalf("применение события: %v", err)
	}
	if !applied {
		t.Fatalf("событие не применено: %s", detail)
	}

	entity, err := (memory.EntityRepo{}).FindOrCreate(ctx, f.pool, "person", "Коля", bgScope)
	if err != nil {
		t.Fatalf("собеседник не создан: %v", err)
	}
	card, err := f.sched.memory.ConversationCard(ctx, bgScope, entity, bgScope, "зарплата")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if !strings.Contains(strings.ToLower(card.Text), "зарплата") {
		t.Fatalf("свежее событие жизни не попало в карточку по релевантному сообщению:\n%s", card.Text)
	}
}

// TestLifeEventRetentionDB — WAL и чекпоинты завершённых bg:life-задач
// вывозятся ретенцией; текущая задача (seq расписания) остаётся живой.
func TestLifeEventRetentionDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedLifeSchedule(t, ctx, 5, f.now.Add(time.Hour))

	oldAge := testCheckpointAge + 24*time.Hour
	f.seedCheckpoint(t, ctx, lifeEventUnitID(1), oldAge)
	f.seedCheckpoint(t, ctx, lifeEventUnitID(5), oldAge)
	f.seedWalRows(t, ctx, lifeEventUnitID(1), testWalAge+24*time.Hour, 1)
	f.seedWalRows(t, ctx, lifeEventUnitID(5), testWalAge+24*time.Hour, 1)

	f.sched.sweepRetention(ctx)

	if f.checkpointExists(t, ctx, lifeEventUnitID(1)) {
		t.Fatal("чекпоинт завершённой bg:life-задачи не вывезен ретенцией")
	}
	if !f.checkpointExists(t, ctx, lifeEventUnitID(5)) {
		t.Fatal("чекпоинт текущей bg:life-задачи удалён ретенцией")
	}
	if got := f.walCount(t, ctx, lifeEventUnitID(1)); got != 0 {
		t.Fatalf("WAL завершённой bg:life-задачи не вывезен: %d строк", got)
	}
	if got := f.walCount(t, ctx, lifeEventUnitID(5)); got == 0 {
		t.Fatal("WAL текущей bg:life-задачи удалён ретенцией")
	}
}
