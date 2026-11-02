// Юнит-тесты решения о инструментах памяти на разговорном ходе (срез 2,
// решение 2026-11-02): целая история + влезшая карточка = один вызов модели
// без инструментов; иначе инструменты без forced и с лимитом 3; откат
// MODEL_MEMORY_PREFETCH=false — прежнее поведение с forced на прямом ходе.
package character

import (
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
