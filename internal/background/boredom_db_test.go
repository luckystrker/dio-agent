//go:build db

// БД-тест скуки (этап 23, гейт этапа): рост шкалы → explore от скуки → память
// → share через обычную политику §14.3; тихие часы не запускают поиск.
// Проверяется проводка такта фона, а не только чистая формула.
package background

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// curiousExploreJSON — находка, которая действительно зацепила: мысль в память
// и зацепка для share (текст «прикинь…»).
const curiousExploreJSON = `{"decision":"answer",` +
	`"messages":["прикинь, у контейнеровозов винт размером с дом"],` +
	`"memory":[{"kind":"thought","text":"контейнеровозы интереснее, чем кажутся","entities":[{"name":"контейнеровозы","kind":"topic"}],"salience":0.6}],` +
	`"reason":"нашёл и правда зацепило"}`

func seedBoredomState(t *testing.T, ctx context.Context, f *fixture, value float64, at time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO character_state (id, boredom, boredom_updated_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE
		  SET boredom = EXCLUDED.boredom, boredom_updated_at = EXCLUDED.boredom_updated_at`,
		value, at); err != nil {
		t.Fatalf("скука не засеяна: %v", err)
	}
}

// TestBoredomExploreShareFlowDB — главный сценарий этапа: скука выше порога
// запускает explore, находка ложится в память, зацепка становится share, и
// share доходит до outbox через обычную политику инициатив.
func TestBoredomExploreShareFlowDB(t *testing.T) {
	f, ctx := newFixture(t)
	// Тема из списка сидов персоны: остальные источники обнулены, чтобы выбор
	// был предсказуем, а генератор — фиксированный.
	f.sched.persona.Interests = nil
	f.sched.persona.SeedBio = nil
	f.sched.persona.Curiosity = []string{"контейнеровозы"}
	f.sched.rnd = rand.New(rand.NewSource(3))
	model := f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: curiousExploreJSON}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
	)

	f.seedPrivateChat(t, ctx, "привет, как дела", f.now.Add(-6*time.Hour))
	seedBoredomState(t, ctx, f, 0.9, f.now)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт со скукой: %v", err)
	}

	// 1. Runtime создал explore от скуки (причина видна в intentions.reason).
	var (
		exploreID int64
		reason    string
		status    string
		topic     string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT id, reason, status, topic FROM intentions WHERE kind = $1 ORDER BY id DESC LIMIT 1`,
		KindExplore).Scan(&exploreID, &reason, &status, &topic); err != nil {
		t.Fatalf("explore-намерение не создано: %v", err)
	}
	if !strings.HasPrefix(reason, boredomReasonPrefix) {
		t.Fatalf("причина explore = %q, хочу метку скуки", reason)
	}
	if topic != "контейнеровозы" {
		t.Fatalf("тема explore = %q, хочу сид персоны «контейнеровозы»", topic)
	}
	if status != "completed" {
		t.Fatalf("explore не исполнился в том же такте: %s", status)
	}

	// 2. Находка в памяти скоупа чата.
	var memories int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE scope_id = $1 AND text LIKE '%контейнеровозы%'`,
		bgScope).Scan(&memories); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if memories != 1 {
		t.Fatalf("находка в памяти: %d записей, хочу 1", memories)
	}

	// 3. Зацепка стала share-намерением (не прямой отправкой).
	var (
		shareID     int64
		shareStatus string
		shareReason string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT id, status, reason FROM intentions WHERE kind = $1 ORDER BY id DESC LIMIT 1`,
		KindShare).Scan(&shareID, &shareStatus, &shareReason); err != nil {
		t.Fatalf("share по находке не создан: %v", err)
	}
	if shareStatus != "pending" {
		t.Fatalf("share должен ждать своей политики, статус %s", shareStatus)
	}
	// Зацепка из explore не теряется: она в reason и попадёт в контекст share.
	if !strings.Contains(shareReason, "зацепка") || !strings.Contains(shareReason, "прикинь") {
		t.Fatalf("в reason share нет зацепки из находки: %q", shareReason)
	}
	var deliveries int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE chat_id = $1`, bgChatID).Scan(&deliveries); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if deliveries != 0 {
		t.Fatalf("explore сам отправил сообщение: %d доставок", deliveries)
	}

	// 4. Скука после находки упала ниже порога: новый поиск не запускается.
	// 5. Share исполняется своим тактом и проходит §14.3: доставка в outbox.
	// Срок share создан по стенным часам (как в проде), поэтому сдвигаем его
	// в прошлое, а не только часы планировщика.
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET not_before = now() - interval '1 minute' WHERE id = $1`, shareID); err != nil {
		t.Fatalf("срок share не сдвинут: %v", err)
	}
	f.setNow(f.now.Add(11 * time.Minute))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт share: %v", err)
	}
	if got := f.intentionStatus(t, ctx, shareID); got != "completed" {
		t.Fatalf("share не исполнился обычной политикой: статус %s", got)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE chat_id = $1 AND status IN ('pending', 'sent')`,
		bgChatID).Scan(&deliveries); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if deliveries == 0 {
		t.Fatal("share не породил доставку")
	}
	// Модель вызвана дважды: explore и share (не больше).
	if calls := len(model.Calls()); calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 (explore + share)", calls)
	}
}

// TestBoredomQuietHoursNoExploreDB — в тихом блоке скука не запускает поиск
// интереса: намерение не создаётся, модель не вызывается.
func TestBoredomQuietHoursNoExploreDB(t *testing.T) {
	f, ctx := newFixture(t)
	loc := moscowLoc(t)
	sched := quietSchedule()
	f.sched.persona.Schedule = &sched
	f.sched.location = loc
	f.sched.persona.Curiosity = []string{"контейнеровозы"}
	f.sched.rnd = rand.New(rand.NewSource(3))

	night := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	f.setNow(night)
	model := f.withModel(t)
	f.seedPrivateChat(t, ctx, "спокойной ночи", night.Add(-2*time.Hour))
	seedBoredomState(t, ctx, f, 0.9, night)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("ночной такт: %v", err)
	}
	var explores int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE kind = $1`, KindExplore).Scan(&explores); err != nil {
		t.Fatalf("намерения не читаются: %v", err)
	}
	if explores != 0 {
		t.Fatalf("в тихий блок создан поиск интереса: %d", explores)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("в тихий блок модель вызвана %d раз", calls)
	}
}

// TestBoredomLiveExploreBlocksSecondDB — пока живо explore-намерение, второй
// поиск интереса не ставится (dedupe по теме — не единственный пояс).
func TestBoredomLiveExploreBlocksSecondDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.sched.persona.Curiosity = []string{"контейнеровозы"}
	f.sched.rnd = rand.New(rand.NewSource(3))
	f.withModel(t)
	f.seedPrivateChat(t, ctx, "привет", f.now.Add(-6*time.Hour))
	seedBoredomState(t, ctx, f, 0.9, f.now)
	f.createIntention(t, ctx, KindExplore, "уже ищу", +1, bgChatID)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	var explores int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE kind = $1`, KindExplore).Scan(&explores); err != nil {
		t.Fatalf("намерения не читаются: %v", err)
	}
	if explores != 1 {
		t.Fatalf("живое explore не остановило второе: %d намерений", explores)
	}
}

// TestBoredomDailyCreatedLimitDB — дневной лимит §16 для триггера считается по
// созданным попыткам, а не только по завершённым: упавшие explore не
// сбрасывают скуку, и без этого счётчика фон ставил бы новый каждые 15–30 с.
func TestBoredomDailyCreatedLimitDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.sched.persona.Interests = nil
	f.sched.persona.SeedBio = nil
	f.sched.persona.Curiosity = []string{"контейнеровозы"}
	f.sched.rnd = rand.New(rand.NewSource(3))
	model := f.withModel(t)
	f.seedPrivateChat(t, ctx, "привет", f.now.Add(-6*time.Hour))
	seedBoredomState(t, ctx, f, 0.9, f.now)

	a := f.createIntention(t, ctx, KindExplore, "упавшая тема 1", -6, bgChatID)
	b := f.createIntention(t, ctx, KindExplore, "упавшая тема 2", -6, bgChatID)
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'failed' WHERE id = ANY($1)`, []int64{a, b}); err != nil {
		t.Fatalf("провалы не засеяны: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	var explores int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE kind = $1`, KindExplore).Scan(&explores); err != nil {
		t.Fatalf("намерения не читаются: %v", err)
	}
	if explores != MaxExplorationsPerDay {
		t.Fatalf("после %d упавших explore создан ещё один: %d намерений", MaxExplorationsPerDay, explores)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("модель вызвана %d раз при исчерпанном лимите", calls)
	}
}

// TestBoredomNoPrivateChatDB — без личного чата писать некому: поиск интереса
// не создаётся (в группы инициативы выключены §14.3).
func TestBoredomNoPrivateChatDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.sched.persona.Interests = nil
	f.sched.persona.SeedBio = nil
	f.sched.persona.Curiosity = []string{"контейнеровозы"}
	f.sched.rnd = rand.New(rand.NewSource(3))
	model := f.withModel(t)
	seedBoredomState(t, ctx, f, 0.9, f.now)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	var explores int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE kind = $1`, KindExplore).Scan(&explores); err != nil {
		t.Fatalf("намерения не читаются: %v", err)
	}
	if explores != 0 {
		t.Fatalf("без личного чата создан поиск интереса: %d", explores)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("без личного чата модель вызвана %d раз", calls)
	}
}
