// Юнит-тесты решения о инструментах памяти на разговорном ходе (срезы 2–3,
// решения 2026-11-02 и 2026-11-03): целая история + полная карточка = один
// вызов модели без инструментов; иначе инструменты без forced и с лимитом 2;
// откат MODEL_MEMORY_PREFETCH=false — прежнее поведение с forced на прямом ходе.
package character

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/luckystrker/dio-agent/internal/memory"
)

func TestPlanMemoryToolsPrefetch(t *testing.T) {
	// Обычный режим, история цела, карточка влезла — инструментов нет.
	plan := planMemoryTools(Input{Direct: true}, BaseContext{MemoryCardFitted: true}, true, true, 5)
	if plan.Enabled || plan.ForcedTool != "" {
		t.Fatalf("целая история + карточка должны идти без инструментов: %+v", plan)
	}

	// История урезана — инструменты есть, forced нет, лимит min(5,3)=3.
	plan = planMemoryTools(Input{Direct: true}, BaseContext{HistoryTrimmed: true, MemoryCardFitted: true}, true, true, 5)
	if !plan.Enabled || plan.ForcedTool != "" || plan.Limit != memoryToolsLimit {
		t.Fatalf("урезанная история: план %+v, хочу enabled без forced, лимит %d", plan, memoryToolsLimit)
	}

	// Карточка не влезла — то же.
	plan = planMemoryTools(Input{Direct: true}, BaseContext{MemoryCardFitted: false}, true, true, 5)
	if !plan.Enabled || plan.ForcedTool != "" || plan.Limit != memoryToolsLimit {
		t.Fatalf("карточка не влезла: план %+v", plan)
	}

	// Настроенный лимит меньше 3 уважается: min(ToolCallsLimit, 3).
	plan = planMemoryTools(Input{}, BaseContext{HistoryTrimmed: true}, true, true, 1)
	if !plan.Enabled || plan.Limit != 1 {
		t.Fatalf("лимит 1 не применён: %+v", plan)
	}
	// 0 = без ограничения в конфиге, но потолок среза 2 всё равно 3.
	plan = planMemoryTools(Input{}, BaseContext{HistoryTrimmed: true}, true, true, 0)
	if plan.Limit != memoryToolsLimit {
		t.Fatalf("0 (без ограничения) не срезан до %d: %+v", memoryToolsLimit, plan)
	}

	// Без инструментария памяти планов нет.
	if plan := planMemoryTools(Input{Direct: true}, BaseContext{MemoryCardFitted: true}, false, true, 5); plan.Enabled {
		t.Fatalf("Memory=nil не должен выдавать инструменты: %+v", plan)
	}
}

func TestPlanMemoryToolsRollback(t *testing.T) {
	// Откат: прежнее поведение — инструменты всегда, forced на прямом ходе,
	// лимит из конфигурации.
	plan := planMemoryTools(Input{Direct: true, Attempt: 1}, BaseContext{MemoryCardFitted: true}, true, false, 7)
	if !plan.Enabled || plan.ForcedTool != memory.ToolSearch || plan.Limit != 7 {
		t.Fatalf("откат: план %+v, хочу forced %s и лимит 7", plan, memory.ToolSearch)
	}
	// Повтор (Attempt>1) не forced.
	plan = planMemoryTools(Input{Direct: true, Attempt: 2}, BaseContext{}, true, false, 7)
	if !plan.Enabled || plan.ForcedTool != "" {
		t.Fatalf("повтор не должен быть forced: %+v", plan)
	}
	// Непрямой ход не forced.
	plan = planMemoryTools(Input{Direct: false}, BaseContext{}, true, false, 7)
	if plan.ForcedTool != "" {
		t.Fatalf("непрямой ход forced: %+v", plan)
	}
}

// TestMemoryToolsReason — этап 21.4: причина включения инструментов в лог
// разговорного хода — history_trimmed / card_incomplete:<причина> / rollback /
// none. С 28.3 причина неполноты карточки уточняется веткой.
func TestMemoryToolsReason(t *testing.T) {
	cases := []struct {
		name     string
		prefetch bool
		bctx     BaseContext
		want     string
	}{
		{"откат", false, BaseContext{}, "rollback"},
		{"история урезана", true, BaseContext{HistoryTrimmed: true, MemoryCardFitted: true}, "history_trimmed"},
		{"карточка неполна", true, BaseContext{MemoryCardFitted: false}, "card_incomplete"},
		{"карточка: срез full-text", true, BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "fts_cut"}, "card_incomplete:fts_cut"},
		{"карточка: бюджет", true, BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "budget"}, "card_incomplete:budget"},
		{"карточка: отношения и бюджет", true, BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "budget,stances"}, "card_incomplete:budget,stances"},
		{"всё на месте", true, BaseContext{MemoryCardFitted: true}, "none"},
	}
	for _, tc := range cases {
		if got := memoryToolsReason(tc.prefetch, tc.bctx); got != tc.want {
			t.Errorf("%s: memoryToolsReason = %q, хочу %q", tc.name, got, tc.want)
		}
	}
}

// TestLogMemoryTurn — этап 21.4: одна строка на ход несёт инструменты/причину,
// карточку и семантику; «модель опять сожгла лимит» отличимо от «карточка
// неполна».
func TestLogMemoryTurn(t *testing.T) {
	var buf bytes.Buffer
	r := &Runner{
		Logger:         log.New(&buf, "", 0),
		Memory:         &memory.Toolset{},
		MemoryPrefetch: true,
	}
	r.logMemoryTurn(Input{TurnID: 42},
		BaseContext{HistoryTrimmed: true, MemoryCardFitted: false, MemoryCardEntries: 4},
		memoryToolPlan{Enabled: true, Limit: memoryToolsLimit})
	line := buf.String()
	for _, want := range []string{
		"ход 42: память",
		"обращение нужно",
		"причина: history_trimmed",
		"записей=4",
		"fits=false",
		"семантика: выключена",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("в строке наблюдаемости нет %q: %s", want, line)
		}
	}
}
