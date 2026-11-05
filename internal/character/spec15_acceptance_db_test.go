//go:build db

// Приёмочные БД-тесты этапа 15.1 (тестировщик; §11.1 п.5, §13.5, §6.3;
// CHECKLIST.md «Этап 15.1»). Написаны по спеке, реализация turn.go/history.go
// не читалась. Фикстура — newFixture/seedTurn/withModel из turn_db_test.go
// (тот же пакет, тег db).
//
// Проверяется на живой базе:
//   - в разговор хода попадают только ДОСТАВЛЕННЫЕ ответы: pending outbox —
//     не состоявшийся опыт (§13.5/§13.3);
//   - окно MODEL_HISTORY_MESSAGES действует на проводе через Runner
//     (первыми отбрасываются самые старые);
//   - низкосигнальные записи чата доступны в недавней истории (§6.3):
//     journal_only и стикер; фото не даёт пустой реплики и сырого JSON.
package character

import (
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

// sp15WireMsg — сообщение на проводе модели: роль + текст.
type sp15WireMsg struct {
	role string // user | assistant
	text string
}

// sp15WireOfCall разворачивает события одного модельного вызова: ModelRequest
// — реплика пользователя, ModelResponse — реплика ассистента (так сеется
// история и так отвечает модель).
func sp15WireOfCall(t *testing.T, evs []events.Event) []sp15WireMsg {
	t.Helper()
	var out []sp15WireMsg
	for _, ev := range evs {
		switch v := events.AsValue(ev).(type) {
		case events.ModelRequest:
			text := ""
			for _, part := range v.Parts {
				if ti, ok := events.AsValue(part).(events.TextInput); ok {
					text += ti.Content
				}
			}
			out = append(out, sp15WireMsg{role: "user", text: text})
		case events.ModelResponse:
			out = append(out, sp15WireMsg{role: "assistant", text: v.Content()})
		}
	}
	return out
}

// TestSpec15PendingAnswerNotInConversation — ход 1 ответил, но ответ НЕ
// доставлен (delivery-loop в тесте не работает): ход 2 видит реплику
// пользователя, но НЕ видит неотправленный ответ (§13.5: «не изображать
// неотправленный текст частью фактического разговора»).
func TestSpec15PendingAnswerNotInConversation(t *testing.T) {
	f, ctx := newFixture(t)

	first := int64(9201)
	f.seedTurn(t, ctx, first, "я решил всё переписать")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{
		Content: `{"decision":"answer","messages":["а что переписываешь?"]}`,
	}})
	if _, err := f.runner.Run(ctx, Input{TurnID: first, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "я решил всё переписать"}}}); err != nil {
		t.Fatalf("ход 1 не выполнен: %v", err)
	}
	var st string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM delivery_outbox WHERE turn_id = $1`, first).Scan(&st); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if st == "sent" {
		t.Fatalf("фикстура сломана: ответ доставлен без delivery-loop (status=sent)")
	}

	second := int64(9202)
	f.seedTurn(t, ctx, second, "ну там пару сервисов")
	cfg := f.withModel(&events.ModelResponse{Message: &events.ModelMessage{
		Content: `{"decision":"answer","messages":["ну удачи)"]}`,
	}})
	if _, err := f.runner.Run(ctx, Input{TurnID: second, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "ну там пару сервисов"}}}); err != nil {
		t.Fatalf("ход 2 не выполнен: %v", err)
	}

	calls := cfg.Calls()
	if len(calls) == 0 {
		t.Fatal("модель не вызывалась")
	}
	wire := sp15WireOfCall(t, calls[len(calls)-1])
	// Ожидание: реплика ходa 1 + пачка ходa 2. Всё остальное — утечка
	// неотправленного опыта в разговор.
	if len(wire) != 2 {
		t.Fatalf("на проводе %d сообщений (%+v), хочу 2: реплика ходa 1 и пачка — pending-ответ просочился в разговор?", len(wire), wire)
	}
	if wire[0].role != "user" || !strings.HasSuffix(wire[0].text, "] #m4242 Коля: я решил всё переписать") {
		t.Fatalf("реплика ходa 1 потеряна (метка/автор): %+v", wire[0])
	}
	if wire[1].role != "user" || !strings.HasPrefix(wire[1].text, statePanelOpen) || !strings.HasSuffix(wire[1].text, "ну там пару сервисов") {
		t.Fatalf("пачка не последняя на проводе — перед ней панель: %+v", wire[1])
	}
	for _, m := range wire {
		if strings.Contains(m.text, "а что переписываешь") {
			t.Fatalf("pending-ответ попал в разговор (§13.5): %+v", wire)
		}
	}
}

// TestSpec15HistoryWindowThroughRunner — окно MODEL_HISTORY_MESSAGES действует
// на проводе: из трёх предыдущих пар (реплика+доставленный ответ) с окном 4
// модель видит только две самые свежие пары, затем пачку (первыми отбрасываются
// самые старые, §11.1 п.5).
func TestSpec15HistoryWindowThroughRunner(t *testing.T) {
	f, ctx := newFixture(t)

	for i := 0; i < 3; i++ {
		turnID := int64(9211 + i)
		text := "реплика номер " + string(rune('1'+i))
		f.seedTurn(t, ctx, turnID, text)
		f.withModel(&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":["ответ номер ` + string(rune('1'+i)) + `"]}`,
		}})
		if _, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
			Batch: []BatchEvent{{EventID: int64(i + 1), ActorID: 777, ActorName: "Коля", Text: text}}}); err != nil {
			t.Fatalf("ход %d не выполнен: %v", i+1, err)
		}
		if _, err := f.pool.Exec(ctx,
			`UPDATE delivery_outbox SET status='sent', telegram_message_id = 560000 + $1 WHERE turn_id = $1`, turnID); err != nil {
			t.Fatalf("outbox ходa %d не доставлен: %v", turnID, err)
		}
	}

	// Окно 4 сообщения = последние две пары: ходы 2 и 3 целиком, ход 1 — за окном.
	f.runner.Assembler.HistoryMessages = 4

	fourth := int64(9214)
	f.seedTurn(t, ctx, fourth, "реплика номер 4")
	cfg := f.withModel(&events.ModelResponse{Message: &events.ModelMessage{
		Content: `{"decision":"answer","messages":["ок"]}`,
	}})
	if _, err := f.runner.Run(ctx, Input{TurnID: fourth, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 4, ActorID: 777, ActorName: "Коля", Text: "реплика номер 4"}}}); err != nil {
		t.Fatalf("ход 4 не выполнен: %v", err)
	}

	calls := cfg.Calls()
	if len(calls) == 0 {
		t.Fatal("модель не вызывалась")
	}
	wire := sp15WireOfCall(t, calls[len(calls)-1])
	want := []sp15WireMsg{
		{role: "user", text: "реплика номер 2"},
		{role: "assistant", text: "ответ номер 2"},
		{role: "user", text: "реплика номер 3"},
		{role: "assistant", text: "ответ номер 3"},
		{role: "user", text: "реплика номер 4"}, // пачка текущего хода
	}
	if len(wire) != len(want) {
		t.Fatalf("на проводе %d сообщений (%+v), хочу %d (окно 4 + пачка)", len(wire), wire, len(want))
	}
	for i, w := range want {
		if wire[i].role != w.role || !strings.HasSuffix(wire[i].text, w.text) {
			t.Fatalf("провод[%d] = %+v, хочу %s с суффиксом %q", i, wire[i], w.role, w.text)
		}
	}
}

// TestSpec15LowSignalEventsInRecentHistory — низкосигнальные записи доступны
// в недавней истории (§6.3): journal_only-событие и стикер (нормализуется
// эмодзи в тексте, решение 2026-10-31) видны; фото не даёт пустой реплики и
// не утекает сырым JSON. Порядок — хронологический.
func TestSpec15LowSignalEventsInRecentHistory(t *testing.T) {
	f, ctx := newFixture(t)
	pool := f.pool
	chat := testChatID

	seed := []struct {
		updateID int64
		kind     string
		text     *string
		raw      string
		status   string
	}{
		{3000001, "message", sp15ptr("текстовое сообщение"), "{}", "processed"},
		{3000002, "sticker", sp15ptr("🔥"), "{}", "journal_only"},
		{3000003, "photo", nil, `{"message":{"photo":[{"file_id":"f1","width":90,"height":90}]}}`, "journal_only"},
		{3000004, "message", sp15ptr("после стикера"), "{}", "buffered"},
		// Второй стикер с обычным статусом: отличает фильтр по статусу
		// (journal_only) от фильтра по типу события (sticker).
		{3000005, "sticker", sp15ptr("😎"), "{}", "processed"},
		// Текстовое сообщение с низкосигнальным статусом: §6.3 требует его
		// доступности в недавней истории независимо от типа события.
		{3000006, "message", sp15ptr("тихая болтовня"), "{}", "journal_only"},
		// Фото С ПОДПИСЬЮ (решение 2026-11-01: «фото с текстом» — в недавней
		// истории; normalize.go: подпись фото хранится в text): отличает
		// включение kind='photo' в SQL от случая text IS NULL у фото без
		// подписи. Находка критика: без этой строки удаление 'photo' из
		// списка kind не ломало ни одного теста.
		{3000007, "photo", sp15ptr("кадр с моря"), `{"message":{"photo":[{"file_id":"f2","width":90,"height":90}],"caption":"кадр с моря"}}`, "processed"},
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx, `
			INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status)
			VALUES ($1, $2, 'private', 777, $3, $4, $5::jsonb, $6)`,
			s.updateID, chat, s.kind, s.text, s.raw, s.status); err != nil {
			t.Fatalf("событие %d не вставлено: %v", s.updateID, err)
		}
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
	// «тихая болтовня» (message, journal_only) проходит в историю — §6.3 для
	// текстовых сообщений выполнен. Стикеры (🔥 journal_only, 😎 processed)
	// отсутствуют при любом статусе — фильтр по kind, находка приёмки (§6.3:
	// низкосигнальные записи доступны в недавней истории; §11.1 п.5: обе
	// стороны; решение 2026-10-31: стикер — kind=sticker с эмодзи в тексте).
	for _, want := range []string{"user:текстовое сообщение", "user:🔥", "user:😎", "user:после стикера", "user:тихая болтовня", "user:кадр с моря"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("в истории нет %q (§6.3/§11.1 п.5): %s", want, joined)
		}
	}
	iMsg := strings.Index(joined, "текстовое сообщение")
	iStick := strings.Index(joined, "🔥")
	iAfter := strings.Index(joined, "после стикера")
	iCaption := strings.Index(joined, "кадр с моря")
	if !(iMsg < iStick && iStick < iAfter && iAfter < iCaption) {
		t.Fatalf("хронология нарушена: %s", joined)
	}
	for _, m := range history {
		if strings.Contains(m.Content, "file_id") {
			t.Fatalf("сырой update утёк в историю: %s", joined)
		}
		if strings.TrimSpace(m.Content) == "" {
			t.Fatalf("пустая реплика в истории (фото без текста?): %s", joined)
		}
	}
}

func sp15ptr(s string) *string { return &s }
