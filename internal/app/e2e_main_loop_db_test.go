//go:build db

// E2E главного продуктового цикла (§1, эталон — сводный сценарий
// BEHAVIOR_SCENARIOS): значимая тема в разговоре → запись с источником →
// exploration → возврат к теме через дни (время двигается) → пересмотр позиции
// при новом противоречащем опыте. Вся цепочка — на живых модулях приложения
// (poller, буферы, worker, фон, доставка) со скриптованной офлайн-моделью.
package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"ago/events"
)

const (
	mlChat  = int64(100501) // личный чат §1
	mlScope = "chat:100501"
	mlTopic = "виниловый проигрыватель"
)

func textResp(body string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: body}}
}

func toolResp(id, name, args string) *events.ModelResponse {
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: id, Name: name, Arguments: args},
	}}}
}

// Глобальная очередь ответов модельных вызовов всего сценария §1 (порядок
// детерминирован: фон откладывается при прямом обращении §5.2, ходы фазы
// разделены ожиданиями применённых результатов).
func mainLoopScript() []*events.ModelResponse {
	return []*events.ModelResponse{
		// 1. Ход прямого обращения: ответ + claim с сущностями + намерение explore.
		textResp(`{"decision":"answer","messages":["винил — правильная тема. главное не экономь на игле."],` +
			`"memory":[{"kind":"claim","text":"Коля собирается купить виниловый проигрыватель и переживает, что они ломаются",` +
			`"entities":[{"name":"виниловый проигрыватель","kind":"topic","role":"subject"},{"name":"Коля","kind":"person","role":"participant"}],"salience":0.7}],` +
			`"intentions":[{"kind":"explore","topic":"надёжность виниловых проигрывателей","reason":"Коля переживает из-за поломок — хочу разобраться сам","delay_hours":0.05}],` +
			`"mood_delta":{"valence":0.2,"reason":"приятная тема"},"reason":"участвую в разговоре"}`),
		// 2. Exploration: обращается к памяти, находит…
		toolResp("c-e1", "memory_search", `{"query":"виниловый проигрыватель надёжность поломки"}`),
		// 3. …и оформляет находку: находка станет share-намерением (§15), не отправкой.
		textResp(`{"decision":"answer","messages":["нашла про винил кое-что — расскажу при случае"],` +
			`"memory":[{"kind":"thought","text":"Прочитала: большинство поломок виниловых проигрывателей — ремень и игла, лечится дёшево",` +
			`"entities":[{"name":"виниловый проигрыватель","kind":"topic","role":"subject"}],"salience":0.6}],` +
			`"reason":"находка заслуживает рассказа"}`),
		// 4. Консолидация эпизода 1: summary + позиция по аспекту (§10.2).
		textResp(`{"summary":"Коля собрался купить виниловый проигрыватель и переживает из-за поломок; обсудили.",` +
			`"stances":[{"entity":"виниловый проигрыватель","aspect":"надёжность","summary":"Настороженно: слышала про поломки, но это поправимо","strength":0.5,"interest":0.6}]}`),
		// 5. Ход инициативы (share): вспоминает тему…
		toolResp("c-i1", "memory_search", `{"query":"виниловый проигрыватель"}`),
		// 6. …и пишет сама (возврат к теме через дни, §1 п.5).
		textResp(`{"decision":"answer","messages":["я кстати всё-таки полезла читать про винил: чаще всего там ремень летает, это копейки"],` +
			`"mood_delta":{"valence":0.1,"reason":"вернулась к теме"},"reason":"срок пришёл — делюсь находкой"}`),
		// 7. Новый опыт: прямой ход — принудительно память (§10.4)…
		toolResp("c-t2", "memory_search", `{"query":"виниловый проигрыватель Коля"}`),
		// 8. …и запись противоречащего опыта (§1 п.6).
		textResp(`{"decision":"answer","messages":["о, здраво! значит зря боялся за поломки?"],` +
			`"memory":[{"kind":"experience","text":"Коля купил виниловый проигрыватель — месяц работает без проблем",` +
			`"entities":[{"name":"виниловый проигрыватель","kind":"topic","role":"subject"},{"name":"Коля","kind":"person","role":"participant"}],"salience":0.8}],` +
			`"relationship_delta":{"affinity":0.2,"trust":0.1,"reason":"разделил радость покупки"},` +
			`"mood_delta":{"valence":0.3,"reason":"рад за собеседника"},"reason":"новый опыт против прежних опасений"}`),
		// 9. Консолидация эпизода 2: пересмотр позиции при новом опыте (§10.3).
		textResp(`{"summary":"Коля купил проигрыватель: месяц без поломок, опасения не подтвердились.",` +
			`"stances":[{"entity":"виниловый проигрыватель","aspect":"надёжность","summary":"Опасения не подтвердились: месяц без проблем","strength":0.8,"interest":0.5}]}`),
	}
}

// stanceRow — текущее состояние stance по сущности-теме.
type stanceRow struct {
	Summary  string
	Revision int64
	Strength float64
}

func (h *harness) stance() (stanceRow, bool) {
	h.t.Helper()
	var row stanceRow
	err := h.pool.QueryRow(context.Background(), `
		SELECT st.summary, st.revision, st.strength::float8
		FROM stances st JOIN entities e ON e.id = st.entity_id
		WHERE e.canonical_name = $1 AND st.scope_id = $2 AND st.aspect = 'надёжность'`, mlTopic, mlScope).
		Scan(&row.Summary, &row.Revision, &row.Strength)
	if err != nil {
		if !strings.Contains(err.Error(), "no rows") {
			h.t.Logf("stance не читается: %v", err)
		}
		return stanceRow{}, false
	}
	return row, true
}

func (h *harness) memoryCount(kind, provKind string) int64 {
	return queryInt(h, `
		SELECT count(*) FROM memory_items mi
		JOIN provenance p ON p.id = mi.provenance_id
		WHERE mi.kind = $1 AND p.kind = $2`, kind, provKind)
}

func (h *harness) memoryProvenanceRef(kind, textLike string) string {
	return queryString(h, `
		SELECT p.ref FROM memory_items mi
		JOIN provenance p ON p.id = mi.provenance_id
		WHERE mi.kind = $1 AND mi.text LIKE '%' || $2 || '%'
		ORDER BY mi.id LIMIT 1`, kind, textLike)
}

func (h *harness) familiarity() float64 {
	h.t.Helper()
	var v float64
	if err := h.pool.QueryRow(context.Background(), `
		SELECT r.familiarity::float8 FROM relationships r
		JOIN entities e ON e.id = r.person_entity_id
		WHERE e.canonical_name = 'Коля' AND r.scope_id = $1`, mlScope).Scan(&v); err != nil {
		h.t.Fatalf("familiarity не читается: %v", err)
	}
	return v
}

func (h *harness) intentionCount(kind, status string) int64 {
	return queryInt(h, `SELECT count(*) FROM intentions WHERE kind = $1 AND status = $2`, kind, status)
}

func (h *harness) sentMessages() []stubCall { return h.api.callsOf("sendMessage") }

// TestMainLoop — полный цикл §1 на живом приложении.
func TestMainLoop(t *testing.T) {
	h := newHarness(t, mainLoopScript()...)

	// --- шаг 1: значимая тема в разговоре ---
	h.api.push(1, mlChat, "private", "слушай, я вот думаю завести виниловый проигрыватель, но говорят они все ломаются")

	// Ответ доставлен, ход применён.
	eventually(t, "ответ хода доставлен", func() bool {
		return len(h.sentMessages()) == 1
	})
	if got := h.sentMessages()[0]; got.ChatID != mlChat || !strings.Contains(got.Text, "винил") {
		t.Fatalf("первый ответ не про то: %+v", got)
	}

	// Шаг 2 (§1 п.2): запись с источником — claim с provenance события хода.
	eventually(t, "claim с источником turn:* записан", func() bool {
		ref := h.memoryProvenanceRef("claim", "виниловый")
		return strings.HasPrefix(ref, "turn:")
	})
	claimRef := h.memoryProvenanceRef("claim", "виниловый")
	t.Logf("claim записан с источником %s", claimRef)

	// Намерение исследования создано ходом (§14.1).
	if h.intentionCount("explore", "pending") != 1 {
		t.Fatalf("explore-намерение не создано: %d", h.intentionCount("explore", "pending"))
	}

	// --- шаг 3 (§1 п.3): exploration; время двигается на дни — эпизод готов ---
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE intentions SET not_before = now() - interval '1 second' WHERE kind = 'explore'`); err != nil {
		t.Fatalf("срок explore не подвинут: %v", err)
	}
	h.clk.set(time.Now().Add(48 * time.Hour)) // «через несколько дней»

	// Exploration применился: находка — thought с источником runtime-действия
	// (§15), и создано share-намерение вместо прямой отправки.
	eventually(t, "share-намерение создано exploration", func() bool {
		return h.intentionCount("share", "pending")+h.intentionCount("share", "completed") >= 1
	})
	if got := h.memoryProvenanceRef("thought", "ремень"); !strings.HasPrefix(got, "intention:") {
		t.Fatalf("thought не с источником намерения: %q", got)
	}

	// Срок share тоже двигаем (10 минут спеки не ждём реальным временем).
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE intentions SET not_before = now() - interval '1 second' WHERE kind = 'share'`); err != nil {
		t.Fatalf("срок share не подвинут: %v", err)
	}

	// --- шаг 4 (§1 п.4): позиция по теме — консолидация применила stance ---
	// --- шаг 5 (§1 п.5): персонаж сама возвращается к теме через дни ---
	eventually(t, "stance эпизода 1 применён и инициатива доставлена", func() bool {
		st, ok := h.stance()
		return ok && strings.Contains(st.Summary, "Настороженно") && len(h.sentMessages()) == 2
	})
	st, _ := h.stance()
	if !strings.Contains(st.Summary, "Настороженно") {
		t.Fatalf("stance эпизода 1 не тот: %+v", st)
	}
	initiative := h.sentMessages()[1]
	if !strings.Contains(initiative.Text, "полезла читать про винил") {
		t.Fatalf("инициатива не про возвращение к теме: %+v", initiative)
	}
	if got := h.intentionCount("share", "completed"); got != 1 {
		t.Fatalf("share-намерение не завершено: %d", got)
	}

	// --- шаг 6 (§1 п.6): новый противоречащий опыт → пересмотр позиции ---
	before := h.familiarity()
	h.api.push(2, mlChat, "private", "а я всё-таки купил его — месяц работает без проблем")

	eventually(t, "ответ на новый опыт доставлен", func() bool {
		return len(h.sentMessages()) == 3
	})
	if !strings.Contains(h.sentMessages()[2].Text, "здраво") {
		t.Fatalf("ответ на новый опыт не тот: %+v", h.sentMessages()[2])
	}

	// Опыт записан с источником хода; отношения выросли (преемственность).
	eventually(t, "experience-запись нового опыта", func() bool {
		ref := h.memoryProvenanceRef("experience", "месяц работает без проблем")
		return strings.HasPrefix(ref, "turn:")
	})
	after := h.familiarity()
	if after <= before {
		t.Fatalf("familiarity не выросла: до %v, после %v", before, after)
	}

	// Консолидация эпизода 2: тишина 4 ч — позиция пересматривается.
	h.clk.set(time.Now().Add(4 * time.Hour).Add(10 * time.Minute))
	eventually(t, "stance пересмотрен новым опытом", func() bool {
		st, ok := h.stance()
		return ok && st.Revision >= 1 && strings.Contains(st.Summary, "не подтвердились")
	})
	st, _ = h.stance()
	if !strings.Contains(st.Summary, "не подтвердились") {
		t.Fatalf("stance не пересмотрен новым опытом: %+v", st)
	}
	// Основания пересмотра — память эпизода 2 (новый опыт, §10.1).
	var basisCount int64
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM stance_basis sb
		JOIN memory_items mi ON mi.id = sb.memory_id
		JOIN stances st ON st.id = sb.stance_id
		JOIN entities e ON e.id = st.entity_id
		WHERE e.canonical_name = $1 AND st.scope_id = $2 AND mi.kind = 'experience'`, mlTopic, mlScope).Scan(&basisCount); err != nil {
		t.Fatalf("основания stance не читаются: %v", err)
	}
	if basisCount == 0 {
		t.Fatalf("в основаниях пересмотра нет нового опыта")
	}

	// Преемственность (§1 п.6: пересмотр с понятной преемственностью):
	// прежний claim и находка живы вместе с новым опытом.
	if n := h.memoryCount("claim", "journal_event"); n != 1 {
		t.Fatalf("claim потерян: %d", n)
	}
	if n := h.memoryCount("thought", "runtime_action"); n != 1 {
		t.Fatalf("thought потерян: %d", n)
	}

	// Хореография модельных вызовов: ровно сценарные (лишний вызов = баг фона).
	if got := h.modelCalls(); got != len(mainLoopScript()) {
		t.Fatalf("модельных вызовов %d, сценарных %d", got, len(mainLoopScript()))
	}
}
