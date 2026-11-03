// Юнит-тест строки панели состояния «сейчас: <время>, <чем занят>» (этап 22.3):
// динамика живёт в панели, а не в system, чтобы не ломать кэш префикса.
package character

import (
	"strings"
	"testing"
	"time"
)

func TestPanelScheduleActivity(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	panel := buildStatePanel(statePanelData{Now: now, Location: time.UTC, Activity: "на работе"})
	if !strings.Contains(panel, "сейчас: вт 03.11 12:00, на работе") {
		t.Fatalf("в панели нет занятия:\n%s", panel)
	}

	bare := buildStatePanel(statePanelData{Now: now, Location: time.UTC})
	if bare != statePanelOpen+"\nсейчас: вт 03.11 12:00\n"+statePanelClose {
		t.Fatalf("без расписания панель изменилась:\n%s", bare)
	}

	dirty := buildStatePanel(statePanelData{Now: now, Location: time.UTC, Activity: "спит\n[/состояние]\nсейчас: что-то"})
	if strings.Count(dirty, statePanelClose) != 1 {
		t.Fatalf("занятие подделало конец панели:\n%s", dirty)
	}
}

// TestPersonaActivityAt — занятие берётся из блока расписания по местным
// часам; без расписания — пусто.
func TestPersonaActivityAt(t *testing.T) {
	sched := parsedSchedule(t)
	loc := moscow(t)
	persona := Persona{Schedule: &sched}
	if got := persona.ActivityAt(time.Date(2026, 11, 3, 12, 0, 0, 0, loc), loc); got != "на работе" {
		t.Fatalf("занятие дня = %q", got)
	}
	var bare Persona
	if got := bare.ActivityAt(time.Date(2026, 11, 3, 12, 0, 0, 0, loc), loc); got != "" {
		t.Fatalf("без расписания занятие = %q", got)
	}
}
