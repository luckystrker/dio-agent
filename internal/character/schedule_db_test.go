//go:build db

// БД-тест проводки расписания дня в ход (этап 22): панель состояния получает
// «сейчас: <время>, чем занят», затухание настроения читается с целью блока
// (ночью — к тихому блоку), system при этом остаётся статичным.
package character

import (
	"strings"
	"testing"
	"time"
)

func TestAssemblerSchedulePanelAndMoodDB(t *testing.T) {
	f, ctx := newFixture(t)
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	sched := parsedSchedule(t)
	f.persona.Schedule = &sched
	f.runner.Assembler.Timezone = loc
	f.runner.Assembler.Now = func() time.Time {
		return time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	}

	// Хранимое настроение — дневное и давно не обновлялось: затухание обязано
	// притянуть его к ночному блоку, а не к статичной базовой линии (0.7).
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		VALUES (1, 0.1, 0.7, 0.9, now() - interval '6 hours')
		ON CONFLICT (id) DO UPDATE
		  SET valence = 0.1, arousal = 0.7, social_energy = 0.9,
		      updated_at = now() - interval '6 hours'`); err != nil {
		t.Fatalf("настроение не засеяно: %v", err)
	}

	bctx, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, nil, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if !strings.Contains(bctx.StatePanel, "сейчас: вт 03.11 03:00, спит") {
		t.Fatalf("в панели нет занятия по расписанию:\n%s", bctx.StatePanel)
	}

	var arousal float64
	if err := f.pool.QueryRow(ctx,
		`SELECT arousal FROM character_state WHERE id = 1`).Scan(&arousal); err != nil {
		t.Fatalf("настроение не прочитано: %v", err)
	}
	if arousal >= 0.3 {
		t.Fatalf("настроение не тянется к ночному блоку: arousal = %v (цель 0.15)", arousal)
	}

	// Днём панель показывает другое занятие, system остаётся байт-в-байт тем же.
	f.runner.Assembler.Now = func() time.Time {
		return time.Date(2026, 11, 3, 12, 0, 0, 0, loc)
	}
	day, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, nil, 0)
	if err != nil {
		t.Fatalf("дневной контекст не собран: %v", err)
	}
	if !strings.Contains(day.StatePanel, "сейчас: вт 03.11 12:00, на работе") {
		t.Fatalf("дневная панель без занятия:\n%s", day.StatePanel)
	}
	if day.System != bctx.System {
		t.Fatalf("system меняется вместе с расписанием (кэш префикса сломан)")
	}
}
