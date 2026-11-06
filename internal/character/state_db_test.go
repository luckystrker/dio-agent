//go:build db

// БД-тесты персоны §7.1 (файл — источник истины, версии), mood §7.2
// (затухание при чтении), отношений §7.3, недавнего разговора 8.1 и сборки
// контекста §11.1.
package character

import (
	"math"
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// TestPersonaVersionsFromFile — отличие файла от активной записи создаёт новую
// версию, совпадение — нет; история сохраняется; активна всегда ровно одна.
func TestPersonaVersionsFromFile(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewPersonaRepo()

	first, changed, err := repo.EnsureFromSource(ctx, f.pool, PersonaSource{
		Name: "Dio", Description: "Описание v1", SpeechStyle: "Кратко.",
		Values:       []string{"честность"},
		MoodBaseline: Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6},
	})
	if err != nil {
		t.Fatalf("первая версия не создана: %v", err)
	}
	if !changed {
		t.Fatalf("первая версия должна быть создана")
	}

	// Тот же источник: новая версия НЕ создаётся.
	same, changed, err := repo.EnsureFromSource(ctx, f.pool, PersonaSource{
		Name: "Dio", Description: "Описание v1", SpeechStyle: "Кратко.",
		Values:       []string{"честность"},
		MoodBaseline: Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6},
	})
	if err != nil || changed {
		t.Fatalf("совпадение создало версию: changed=%v err=%v", changed, err)
	}
	if same.Version != first.Version {
		t.Fatalf("вернулась чужая версия %d, хочу %d", same.Version, first.Version)
	}

	// Изменённый источник: новая версия, прежняя остаётся в истории.
	_, changed, err = repo.EnsureFromSource(ctx, f.pool, PersonaSource{
		Name: "Dio", Description: "Описание v2", SpeechStyle: "Кратко.",
		Values:       []string{"честность"},
		MoodBaseline: Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6},
	})
	if err != nil || !changed {
		t.Fatalf("отличие файла не создало версию: changed=%v err=%v", changed, err)
	}

	var versions int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM personas`).Scan(&versions); err != nil {
		t.Fatalf("версии не читаются: %v", err)
	}
	// 3 = версия по умолчанию из Active() + две от источников (история сохраняется).
	if versions != 3 {
		t.Fatalf("версий %d, хочу 3 (дефолт + две версии файла)", versions)
	}
	var active int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM personas WHERE active = TRUE`).Scan(&active); err != nil {
		t.Fatalf("активные не читаются: %v", err)
	}
	if active != 1 {
		t.Fatalf("активных персон %d, хочу ровно одну", active)
	}
	current, err := repo.Active(ctx, f.pool)
	if err != nil {
		t.Fatalf("активная не читается: %v", err)
	}
	if current.Description != "Описание v2" {
		t.Fatalf("активная версия устарела: %q", current.Description)
	}
}

// TestPersonaSeedBioNilVsEmpty — отсутствие биографии и пустая строка различимы
// (persona_seed §7.1), совпадение по nil не создаёт версий зря.
func TestPersonaSeedBioNilVsEmpty(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewPersonaRepo()
	base := PersonaSource{
		Name: "Dio", Description: "d", SpeechStyle: "s", Values: []string{"v"},
		MoodBaseline: Mood{},
	}
	if _, changed, err := repo.EnsureFromSource(ctx, f.pool, base); err != nil || !changed {
		t.Fatalf("первая версия: %v %v", changed, err)
	}
	if _, changed, _ := repo.EnsureFromSource(ctx, f.pool, base); changed {
		t.Fatalf("тот же источник без биографии создал версию")
	}
	withBio := base
	bio := "Вымысел"
	withBio.SeedBio = &bio
	if _, changed, err := repo.EnsureFromSource(ctx, f.pool, withBio); err != nil || !changed {
		t.Fatalf("появление биографии не создало версию: %v %v", changed, err)
	}
	if _, changed, _ := repo.EnsureFromSource(ctx, f.pool, withBio); changed {
		t.Fatalf("тот же источник с биографией создал версию")
	}
}

// TestPersonaInterestsRoundTrip — секция «Интересы» доезжает в БД и обратно
// (§7.1, решение 2026-10-31): появление создаёт версию, совпадение — нет.
func TestPersonaInterestsRoundTrip(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewPersonaRepo()
	base := PersonaSource{
		Name: "Dio", Description: "d", SpeechStyle: "s", Values: []string{"v"},
		MoodBaseline: Mood{},
	}
	if _, changed, err := repo.EnsureFromSource(ctx, f.pool, base); err != nil || !changed {
		t.Fatalf("первая версия: %v %v", changed, err)
	}
	withInterests := base
	withInterests.Interests = []string{"старое железо", "море"}
	p, changed, err := repo.EnsureFromSource(ctx, f.pool, withInterests)
	if err != nil || !changed {
		t.Fatalf("появление интересов не создало версию: %v %v", changed, err)
	}
	if len(p.Interests) != 2 || p.Interests[0] != "старое железо" || p.Interests[1] != "море" {
		t.Fatalf("интересы не прочитались из БД: %v", p.Interests)
	}
	if _, changed, _ := repo.EnsureFromSource(ctx, f.pool, withInterests); changed {
		t.Fatalf("тот же источник с интересами создал версию")
	}
	p, err = repo.Active(ctx, f.pool)
	if err != nil || len(p.Interests) != 2 {
		t.Fatalf("активная персона без интересов: %v %v", p.Interests, err)
	}
}

// TestMoodReadDecayedAndApplyDelta — чтение затухает к baseline, дельта ложится
// на затухнувшую базу и сжимается до лимита (§7.2).
func TestMoodReadDecayedAndApplyDelta(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewMoodRepo()
	baseline := Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6}

	// Пустая база: базовая линия.
	mood, _, err := repo.ReadDecayed(ctx, f.pool, baseline)
	if err != nil {
		t.Fatalf("чтение пустого состояния упало: %v", err)
	}
	if mood != baseline {
		t.Fatalf("пустая база вернула %+v, хочу baseline", mood)
	}

	// Устаревшая запись сильно затухает к baseline.
	old := Mood{Valence: 0.9, Arousal: 0.0, SocialEnergy: 1.0}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		 VALUES (1, $1, $2, $3, now() - interval '24 hours')`,
		old.Valence, old.Arousal, old.SocialEnergy); err != nil {
		t.Fatalf("состояние не вставлено: %v", err)
	}
	mood, _, err = repo.ReadDecayed(ctx, f.pool, baseline)
	if err != nil {
		t.Fatalf("чтение упало: %v", err)
	}
	if mood.Valence > old.Valence-0.3 || mood.Valence < baseline.Valence {
		t.Fatalf("valence не затух к baseline: %v", mood.Valence)
	}
	// Чтение записало затухнувшее значение: повторное чтение почти не меняет
	// (расхождение — плавающая точка на мгновенном интервале).
	again, _, err := repo.ReadDecayed(ctx, f.pool, baseline)
	if err != nil {
		t.Fatalf("повторное чтение упало: %v", err)
	}
	if math.Abs(again.Valence-mood.Valence) > 1e-6 {
		t.Fatalf("чтение не зафиксировало затухание: %v против %v", again.Valence, mood.Valence)
	}

	// Репозиторий применяет дельту как дано (сжатие до лимита делает
	// ClampMoodDelta на пути хода, как в TS): 0.9 сверх базы упирается в
	// диапазон [-1..1].
	v := 0.9
	if _, err := repo.ApplyDelta(ctx, f.pool, baseline, MoodDelta{Valence: &v, Reason: "тест"}); err != nil {
		t.Fatalf("дельта не применилась: %v", err)
	}
	var stored float64
	if err := f.pool.QueryRow(ctx, `SELECT valence FROM character_state WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if stored != 1.0 {
		t.Fatalf("valence = %v, хочу 1.0 (диапазон держится)", stored)
	}

	// Сжатая дельта на пути хода: ClampMoodDelta до ApplyDelta даёт baseline+0.3.
	// Свежая база: предыдущий шаг упёрся в потолок 1.0 — сбрасываем состояние.
	if _, err := f.pool.Exec(ctx, `DELETE FROM character_state WHERE id = 1`); err != nil {
		t.Fatalf("состояние не сброшено: %v", err)
	}
	cv := 0.9
	clamped := ClampMoodDelta(MoodDelta{Valence: &cv, Reason: "тест"})
	if _, err := repo.ApplyDelta(ctx, f.pool, baseline, clamped); err != nil {
		t.Fatalf("сжатая дельта не применилась: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT valence FROM character_state WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if stored != baseline.Valence+MaxMoodDeltaPerTurn {
		t.Fatalf("valence = %v, хочу baseline+0.3", stored)
	}
}

// TestRelationshipsApplyDeltaClampAndFamiliarity — дельты сжимаются до лимитов,
// диапазоны держатся, familiarity растёт и не выше 1 (§7.3).
func TestRelationshipsApplyDeltaClampAndFamiliarity(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewRelationshipRepo()

	// Сущность человека.
	var entityID int64
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', 'Коля', $1) RETURNING id`,
		testScopeID).Scan(&entityID); err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}

	// Отсутствие записи — нейтральное стартовое (trust 0.2).
	rel, err := repo.Read(ctx, f.pool, entityID, testScopeID)
	if err != nil {
		t.Fatalf("чтение отсутствующей записи упало: %v", err)
	}
	if rel.Trust != 0.2 || rel.Affinity != 0 || rel.Familiarity != 0 {
		t.Fatalf("стартовое отношение неверное: %+v", rel)
	}

	aff := 0.5 // сжать до 0.1
	trust := -0.4
	rel, err = repo.ApplyDelta(ctx, f.pool, entityID, testScopeID, &aff, &trust)
	if err != nil {
		t.Fatalf("дельта не применилась: %v", err)
	}
	if rel.Affinity != 0.1 {
		t.Fatalf("affinity = %v, хочу сжатые 0.1", rel.Affinity)
	}
	if rel.Trust != 0.2-RelationshipLimits.TrustPerTurn {
		t.Fatalf("trust = %v, хочу 0.2-0.1", rel.Trust)
	}

	// Диапазоны держатся: affinity не ниже -1.
	aff = -2.0
	rel, err = repo.ApplyDelta(ctx, f.pool, entityID, testScopeID, &aff, nil)
	if err != nil {
		t.Fatalf("дельта не применилась: %v", err)
	}
	if rel.Affinity != 0.1-RelationshipLimits.AffinityPerTurn {
		t.Fatalf("affinity = %v, хочу 0.0", rel.Affinity)
	}

	// Familiarity растёт порциями и не выше 1.
	for i := 0; i < 25; i++ {
		if _, err := repo.GrowFamiliarity(ctx, f.pool, entityID, testScopeID,
			RelationshipLimits.FamiliarityPerSignificantTurn); err != nil {
			t.Fatalf("familiarity не выросла: %v", err)
		}
	}
	rel, _ = repo.Read(ctx, f.pool, entityID, testScopeID)
	if rel.Familiarity > 1 {
		t.Fatalf("familiarity выше 1: %v", rel.Familiarity)
	}
}

// TestRecentHistoryBothSidesAndTurnExclusion — обе стороны, только доставленные
// ответы, события текущего хода исключены (§11.1 п.5, §13.3).
func TestRecentHistoryBothSidesAndTurnExclusion(t *testing.T) {
	f, ctx := newFixture(t)
	pool := f.pool
	chat := testChatID

	// Ход 1: входящее + доставленный ответ.
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, turn_id)
		VALUES (1, $1, 'private', 777, 'message', 'вопрос первый', '{}'::jsonb, 'processed', 11)`, chat); err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status)
		VALUES (11, 0, $1, '{"kind":"message","text":"ответ первый"}'::jsonb, 'sent')`, chat); err != nil {
		t.Fatalf("outbox не вставлен: %v", err)
	}
	// Ход 2: pending-ответ не должен попасть в разговор (§13.3: pending — не опыт).
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status)
		VALUES (12, 0, $1, '{"kind":"message","text":"ответ не ушёл"}'::jsonb, 'pending')`, chat); err != nil {
		t.Fatalf("outbox не вставлен: %v", err)
	}
	// Ещё входящее без turn_id.
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status)
		VALUES (2, $1, 'private', 777, 'message', 'вопрос второй', '{}'::jsonb, 'buffered')`, chat); err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}

	history, err := RecentHistory(ctx, pool, chat, 1000, 0)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	var texts []string
	for _, m := range history {
		texts = append(texts, m.Role+":"+m.Content)
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "user:вопрос первый") || !strings.Contains(joined, "assistant:ответ первый") {
		t.Fatalf("обе стороны не в истории: %s", joined)
	}
	if strings.Contains(joined, "ответ не ушёл") {
		t.Fatalf("pending-ответ попал в разговор (§13.3): %s", joined)
	}

	// Хронологический порядок.
	if history[0].Content != "вопрос первый" || history[len(history)-1].Content != "вопрос второй" {
		t.Fatalf("порядок истории нарушен: %s", joined)
	}

	// Окно: 1 сообщение — только самое свежее.
	short, err := RecentHistory(ctx, pool, chat, 1, 0)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	if len(short) != 1 || short[0].Content != "вопрос второй" {
		t.Fatalf("окно 1 вернуло %+v, хочу только свежее", short)
	}

	// Исключение текущего хода.
	excluded, err := RecentHistory(ctx, pool, chat, 1000, 11)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	for _, m := range excluded {
		if m.Content == "вопрос первый" || m.Content == "ответ первый" {
			t.Fatalf("события текущего хода не исключены: %+v", m)
		}
	}
}

// TestRecentHistoryExcludesPendingUserEvents — 18.2: событие, ещё сидящее в
// буфере (pending, turn_id IS NULL), не попадает в «недавний разговор»
// текущего хода — иначе оно приходит дважды (пачкой своего хода и историей),
// и модель реагирует на сообщение до его очереди. После закрытия пачки
// (buffered + turn_id) событие появляется в истории следующего хода.
func TestRecentHistoryExcludesPendingUserEvents(t *testing.T) {
	f, ctx := newFixture(t)
	pool := f.pool
	chat := testChatID

	// pending-событие в живом буфере (пачка ещё не закрыта).
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status)
		VALUES (10, $1, 'private', 777, 'message', 'ещё в буфере', '{}'::jsonb, 'pending')`, chat); err != nil {
		t.Fatalf("pending-событие не вставлено: %v", err)
	}
	// Закрытая пачка чужого хода — та обязана быть в истории.
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, turn_id)
		VALUES (11, $1, 'private', 777, 'message', 'закрытая реплика', '{}'::jsonb, 'processed', 33)`, chat); err != nil {
		t.Fatalf("processed-событие не вставлено: %v", err)
	}

	history, err := RecentHistory(ctx, pool, chat, 1000, 0)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	for _, m := range history {
		if m.Content == "ещё в буфере" {
			t.Fatalf("pending-событие просочилось в историю хода (дубль контекста): %+v", m)
		}
	}
	found := false
	for _, m := range history {
		if m.Content == "закрытая реплика" {
			found = true
		}
	}
	if !found {
		t.Fatalf("закрытая реплика пропала из истории: %+v", history)
	}

	// Пачка закрылась в ход 33: событие — в истории следующего хода
	// (exclude 0), но исключено из истории самого хода 33.
	if _, err := pool.Exec(ctx, `
		UPDATE social_events SET turn_id = 33, status = 'buffered' WHERE telegram_update_id = 10`); err != nil {
		t.Fatalf("пачка не закрыта: %v", err)
	}
	history, err = RecentHistory(ctx, pool, chat, 1000, 0)
	if err != nil {
		t.Fatalf("история после закрытия не читается: %v", err)
	}
	reappeared := false
	for _, m := range history {
		if m.Content == "ещё в буфере" {
			reappeared = true
		}
	}
	if !reappeared {
		t.Fatalf("после закрытия пачки события нет в истории следующего хода: %+v", history)
	}
	history, err = RecentHistory(ctx, pool, chat, 1000, 33)
	if err != nil {
		t.Fatalf("история хода 33 не читается: %v", err)
	}
	for _, m := range history {
		if m.Content == "ещё в буфере" {
			t.Fatalf("событие хода 33 не исключено из его собственной истории: %+v", m)
		}
	}
}

// TestAssembleContext — секции §11.1: persona в system, mood-строка,
// отношение к собеседнику, ограничения отдельной секцией, batch с плейсхолдером.
func TestAssembleContext(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 210, "Привет, как погода?")

	assembler := &Assembler{
		Mood:            NewMoodRepo(),
		Relationships:   NewRelationshipRepo(),
		HistoryMessages: 1000,
		HistoryMaxChars: 200000,
	}
	bctx, err := assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID,
		[]BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Привет, как погода?"}}, 210)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}

	if !strings.Contains(bctx.System, "Ты — Dio") {
		t.Fatalf("persona не в system: %s", bctx.System)
	}
	// Решение 2026-11-02: настроение и отношение — в панели, не в system.
	if strings.Contains(bctx.System, "в ровном настроении") || strings.Contains(bctx.System, "почти незнаком") {
		t.Fatalf("динамика вернулась в system: %s", bctx.System)
	}
	if !strings.Contains(bctx.StatePanel, "твоё состояние: "+bctx.MoodLine) {
		t.Fatalf("mood-строки нет в панели: %s", bctx.StatePanel)
	}
	if !strings.Contains(bctx.StatePanel, "отношение к собеседнику: "+bctx.RelationshipLine) {
		t.Fatalf("отношения нет в панели: %s", bctx.StatePanel)
	}
	if !strings.Contains(bctx.Constraints, "Ограничения хода") || !strings.Contains(bctx.Constraints, "дельты") {
		t.Fatalf("constraints-секции нет: %s", bctx.Constraints)
	}
	// Тексты пачки не подмешиваются в constraints (§17: недоверенные данные
	// не в инструкциях).
	if strings.Contains(bctx.Constraints, "погода") {
		t.Fatalf("данные пачки попали в constraints: %s", bctx.Constraints)
	}
	if len(bctx.Batch) != 1 || bctx.Batch[0] != "Привет, как погода?" {
		t.Fatalf("пачка не в контексте: %+v", bctx.Batch)
	}
	// Сущность собеседника создана: ключ — actor_id, в панели — имя.
	if bctx.Actor == nil || !strings.HasPrefix(bctx.Actor.CanonicalName, "user:") {
		t.Fatalf("актор не разрешён по actor_id: %+v", bctx.Actor)
	}
	if bctx.ActorLine != "Коля" {
		t.Fatalf("отображаемое имя собеседника потеряно: %q", bctx.ActorLine)
	}
	// Память в базовый контекст v0.7 не кладётся (§11.1): абзац {{memory}}
	// исчезает из system-промпта, модель добирает её инструментами.
	if strings.Contains(bctx.System, "долговременная память") {
		t.Fatalf("память попала в базовый контекст: %s", bctx.System)
	}
}

// TestAssembleContextMultiActorConstraint — несколько участников пачки дают
// отдельную строку ограничений (§11.1: контекст группы учитывает нескольких).
func TestAssembleContextMultiActorConstraint(t *testing.T) {
	f, ctx := newFixture(t)
	assembler := &Assembler{Mood: NewMoodRepo(), Relationships: NewRelationshipRepo(),
		HistoryMessages: 10, HistoryMaxChars: 1000}
	bctx, err := assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID,
		[]BatchEvent{
			{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "раз"},
			{EventID: 2, ActorID: 888, ActorName: "Аня", Text: "два"},
		}, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if !strings.Contains(bctx.StatePanel, "в пачке несколько участников") {
		t.Fatalf("ограничение пачки не отмечено в панели: %s", bctx.StatePanel)
	}
	if strings.Contains(bctx.Constraints, "несколько участников") {
		t.Fatalf("динамическая строка осталась в constraints: %s", bctx.Constraints)
	}
	// Пустой текст заменяется плейсхолдером (паритет TS).
	bctx2, err := assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID,
		[]BatchEvent{{EventID: 3, ActorID: 777, Text: "  "}}, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if bctx2.Batch[0] != "[сообщение без текста]" {
		t.Fatalf("пустой текст не заменён: %+v", bctx2.Batch)
	}
}

// TestAssembleContextActorNameInSystem — 17.10: имя собеседника доезжает
// до модели — в личке оно строкой в базовом контексте (system). Раньше Actor
// заполнялся, но имя никуда не шло: модель не знала, с кем говорит.
func TestAssembleContextActorNameInSystem(t *testing.T) {
	f, ctx := newFixture(t)
	assembler := &Assembler{Mood: NewMoodRepo(), Relationships: NewRelationshipRepo(),
		HistoryMessages: 10, HistoryMaxChars: 1000}
	bctx, err := assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID,
		[]BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if !strings.Contains(bctx.StatePanel, "ты говоришь с: Коля") {
		t.Fatalf("имя собеседника не в панели состояния:\n%s", bctx.StatePanel)
	}
	if strings.Contains(bctx.System, "Коля") {
		t.Fatalf("имя собеседника просочилось в system:\n%s", bctx.System)
	}
	// Группа с несколькими авторами: имена различаются префиксами пачки,
	// а «Ты говоришь с» одного имени не утверждает (пусто — абзац исчезает).
	bctx, err = assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID,
		[]BatchEvent{
			{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "раз"},
			{EventID: 3, ActorID: 888, ActorName: "Аня", Text: "два"},
		}, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if strings.Contains(bctx.System, "Ты говоришь с") {
		t.Fatalf("в пачке нескольких авторов строка «Ты говоришь с» вводит в заблуждение:\n%s", bctx.System)
	}
	if len(bctx.Batch) != 2 || bctx.Batch[0] != "Коля: раз" || bctx.Batch[1] != "Аня: два" {
		t.Fatalf("реплики группы без префиксов авторов: %v", bctx.Batch)
	}
}

// TestTurnDirectQuestionToolRound — прямой вопрос: ход проходит с
// инструментным кругом памяти (forced tool-calling провода проверяется юнитом
// TestChatClientForcedToolChoiceFirstCallOnly); тут — что ход с forced-конфигом
// и memory_scope DI выполняется и память находит личное (§9.2+§10.4).
func TestTurnDirectQuestionToolRound(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(211)
	f.seedTurn(t, ctx, turnID, "Что я люблю?")

	f.withModel(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: memory.ToolSearch, Arguments: `{"query":"люби пластинки"}`},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["Ты любишь старые пластинки."]}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Direct: true,
		Batch:  []BatchEvent{{EventID: 1, ActorID: 777, Text: "Что я люблю?"}}})
	if err != nil {
		t.Fatalf("прямой ход не выполнен: %v", err)
	}
	if out.Skipped {
		t.Fatalf("прямой ход пропущен: %s", out.SkipReason)
	}
	// Инструментный круг зафиксирован в журнале (§12.1).
	types := f.eventTypes(t, ctx, turnID)
	if !contains(types, EventTypeToolCall) || !contains(types, EventTypeToolResult) {
		t.Fatalf("инструментного круга нет в журнале: %v", types)
	}
	// Ответ применён.
	var text string
	if err := f.pool.QueryRow(ctx, `SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "Ты любишь старые пластинки." {
		t.Fatalf("текст неверен: %q", text)
	}
}

// TestTurnToolLimitExceeded — лимит обращений к инструментам за ход (§11.2):
// лишний вызов отклоняется ошибкой инструмента, ход завершается финальным ответом.
func TestTurnToolLimitExceeded(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.ToolCallsLimit = 1
	turnID := int64(212)
	f.seedTurn(t, ctx, turnID, "поищи дважды")

	f.withModel(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: memory.ToolSearch, Arguments: `{"query":"пластинки"}`},
		}}},
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c2", Name: memory.ToolSearch, Arguments: `{"query":"ещё"}`},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["ответил со второго раза"]}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "поищи дважды"}}})
	if err != nil {
		t.Fatalf("ход с лимитом упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}
	// В журнале два tool_call (оба attempted), второй — с ошибкой лимита.
	calls, results := 0, 0
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case ToolCallRecorded:
			calls++
		case ToolResultRecorded:
			results++
			if calls == 2 && e.Error == "" {
				t.Fatalf("второй вызов должен быть отклонён лимитом")
			}
		}
	}
	if calls != 2 || results != 2 {
		t.Fatalf("журнал инструментов: calls=%d results=%d, хочу 2/2", calls, results)
	}
}

// toolRoundResp — ответ модели с одним обращением к памяти (для серий кругов).
func toolRoundResp(id, query string) *events.ModelResponse {
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: id, Name: memory.ToolSearch, Arguments: `{"query":"` + query + `"}`},
	}}}
}

// TestTurnToolLimitStopCrane — стоп-кран (39.1): первый отказ лимита закрывает
// инструменты (tool_choice=none) и следующий вызов модели — финальный. Если
// модель игнорирует закрытие и снова зовёт инструмент, ход замыкается
// принудительно (ResponseForce), платные круги над отказами не крутятся. Ход
// завершается как ignore: без сообщений пользователю.
func TestTurnToolLimitStopCrane(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.ToolCallsLimit = 1
	turnID := int64(213)
	f.seedTurn(t, ctx, turnID, "поищи много раз")

	// c1 — в лимите; c2 — отказ и закрытие инструментов; c3 — модель игнорирует
	// tool_choice=none, срабатывает кран. Сценария на четвёртый вызов нет
	// намеренно: без стоп-крана прогон исчерпает сценарий и ход упадёт.
	script := f.withModel(
		toolRoundResp("c1", "пластинки"),
		toolRoundResp("c2", "ещё"),
		toolRoundResp("c3", "и ещё"),
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "поищи много раз"}}})
	if err != nil {
		t.Fatalf("ход со стоп-краном упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}
	// Модельных вызовов ровно три: разрешённый круг, отказ, кран; четвёртого
	// платного круга над отказами не случилось.
	if calls := len(script.Calls()); calls != 3 {
		t.Fatalf("модельных вызовов %d, хочу 3 — стоп-кран не ограничил серию", calls)
	}
	if out.LLMCalls != 3 {
		t.Fatalf("LLMCalls = %d, хочу 3", out.LLMCalls)
	}
	// Стоп-кран завершает ход молча: доставки нет.
	var outbox int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outbox); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outbox != 0 {
		t.Fatalf("стоп-кран не должен ставить сообщения в доставку, outbox=%d", outbox)
	}
	// Журнал: три обращения, отказ лимита один (c2), финал стоп-крана —
	// результат, а не ошибка.
	calls, limitErrs, crane := 0, 0, false
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case ToolCallRecorded:
			calls++
		case ToolResultRecorded:
			if e.Error != "" {
				limitErrs++
			}
			if strings.Contains(e.Result, "стоп-кран") {
				crane = true
			}
		}
	}
	if calls != 3 || limitErrs != 1 || !crane {
		t.Fatalf("журнал: calls=%d отказов_лимита=%d стоп_кран=%v, хочу 3/1/true", calls, limitErrs, crane)
	}
}

// TestTurnToolLimitStopCraneDirectReacts — прямой ход на стоп-кране (модель
// игнорирует tool_choice=none) отдаёт видимую реакцию «🤔» (39.1): финальный
// вызов без инструментов не дал текста, а молчаливый ignore потерял бы вопрос.
// Reaction попадает в outbox как отдельный пузырь, сообщений нет.
func TestTurnToolLimitStopCraneDirectReacts(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.ToolCallsLimit = 1
	turnID := int64(216)
	f.seedTurn(t, ctx, turnID, "ответь на прямой вопрос")

	script := f.withModel(
		toolRoundResp("c1", "пластинки"),
		toolRoundResp("c2", "ещё"),
		toolRoundResp("c3", "и ещё"),
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID, Direct: true,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ответь на прямой вопрос"}}})
	if err != nil {
		t.Fatalf("прямой ход со стоп-краном упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}
	if calls := len(script.Calls()); calls != 3 {
		t.Fatalf("модельных вызовов %d, хочу 3", calls)
	}
	var kind, reaction string
	if err := f.pool.QueryRow(ctx,
		`SELECT payload->>'kind', payload->>'reaction' FROM delivery_outbox WHERE turn_id = $1`,
		turnID).Scan(&kind, &reaction); err != nil {
		t.Fatalf("reaction не в outbox: %v", err)
	}
	if kind != "reaction" || reaction != "🤔" {
		t.Fatalf("стоп-кран прямого хода: kind=%q reaction=%q, хочу reaction/🤔", kind, reaction)
	}
}

// TestTurnToolLimitStopCraneBatch — стоп-кран срабатывает и внутри одной пачки
// вызовов (16.5): упорствующая модель может запросить несколько инструментов
// одним ответом; когда третий подряд отказ лимита приходится на вызов из пачки,
// executor ago получает финальный результат и замыкает ход без следующего
// модельного вызова. Порядок исполнения пачки параллелен, поэтому детерминированы
// счётчики (2 отказа + кран), а не имена вызовов.
func TestTurnToolLimitStopCraneBatch(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.ToolCallsLimit = 1
	turnID := int64(214)
	f.seedTurn(t, ctx, turnID, "поищи пачкой")

	// Круг 1 — один вызов в лимите; круг 2 — пачка из четырёх: все сверх лимита,
	// серия доходит до крана прямо внутри пачки. Сценария на третий модельный
	// вызов нет намеренно: без стоп-крана прогон исчерпает сценарий.
	script := f.withModel(
		toolRoundResp("c1", "пластинки"),
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c2", Name: memory.ToolSearch, Arguments: `{"query":"а"}`},
			{ID: "c3", Name: memory.ToolSearch, Arguments: `{"query":"б"}`},
			{ID: "c4", Name: memory.ToolSearch, Arguments: `{"query":"в"}`},
			{ID: "c5", Name: memory.ToolSearch, Arguments: `{"query":"г"}`},
		}}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "поищи пачкой"}}})
	if err != nil {
		t.Fatalf("ход со стоп-краном в пачке упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}
	// Модельных вызовов ровно два: разрешённый круг + пачка, с которой пришёл
	// кран; третьего платного круга нет.
	if calls := len(script.Calls()); calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 — кран не сработал в пачке", calls)
	}
	if out.LLMCalls != 2 {
		t.Fatalf("LLMCalls = %d, хочу 2", out.LLMCalls)
	}
	var outbox int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outbox); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outbox != 0 {
		t.Fatalf("стоп-кран не должен ставить сообщения в доставку, outbox=%d", outbox)
	}
	// Журнал: пять обращений (c1 + пачка), отказ лимита один (первый вызов
	// пачки закрывает инструменты), остальные вызовы пачки замыкаются краном.
	calls, limitErrs, cranes := 0, 0, 0
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case ToolCallRecorded:
			calls++
		case ToolResultRecorded:
			if e.Error != "" {
				limitErrs++
			}
			if strings.Contains(e.Result, "стоп-кран") {
				cranes++
			}
		}
	}
	// Пачка из четырёх сверх лимита: первый — отказ, остальные — кран (какие
	// именно вызовы — решает планировщик горутин, число детерминировано).
	if calls != 5 || limitErrs < 1 || cranes < 1 {
		t.Fatalf("журнал: calls=%d отказов_лимита=%d кранов=%d, хочу 5/>=1/>=1", calls, limitErrs, cranes)
	}
}

// TestTurnToolLimitClosesThenAnswers — 39.1: первый отказ закрывает инструменты,
// и следующий вызов модели идёт с tool_choice=none; модель отвечает текстом, и
// ответ доставляется (закрытие инструментов не превращает прямой вопрос в
// молчание).
func TestTurnToolLimitClosesThenAnswers(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.ToolCallsLimit = 1
	turnID := int64(215)
	f.seedTurn(t, ctx, turnID, "поищи и ответь")

	script := f.withModel(
		toolRoundResp("c1", "пластинки"),
		toolRoundResp("c2", "ещё"),
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["вот ответ"]}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "поищи и ответь"}}})
	if err != nil {
		t.Fatalf("ход с отказом упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}
	if calls := len(script.Calls()); calls != 3 {
		t.Fatalf("модельных вызовов %d, хочу 3 (tool + отказ + финальный текст)", calls)
	}
	var text string
	if err := f.pool.QueryRow(ctx, `SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "вот ответ" {
		t.Fatalf("ответ модели не доставлен: %q", text)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
