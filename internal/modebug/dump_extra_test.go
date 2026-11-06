// Дополнительные тесты дампа (40.1): граничные случаи каталога, ретенции и
// имён файлов. Отладочная функция не должна ни падать на плохом пути, ни
// писать мимо каталога, ни удалять чужие файлы.
package modebug

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNewDumperRejectsFile — путь занят файлом: дампер не создаётся, ошибка
// объясняет причину (приложение выключит дамп, а не упадёт).
func TestNewDumperRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("файл-заглушка не создан: %v", err)
	}
	d, err := NewDumper(path, nil)
	if err == nil {
		t.Fatal("каталог дампа на месте файла создан — ожидаю ошибку")
	}
	if d != nil {
		t.Fatalf("при ошибке дампер должен быть nil: %+v", d)
	}
	if !strings.Contains(err.Error(), "не создан") {
		t.Fatalf("ошибка не называет каталог: %v", err)
	}
}

// TestDumperNilSafe — nil-дампер (MODEL_DEBUG_DUMP_DIR пуст) безопасен во
// всех методах: выключенный дамп не должен требовать проверок у вызывающего.
func TestDumperNilSafe(t *testing.T) {
	var d *Dumper
	if got := d.Dir(); got != "" {
		t.Fatalf("Dir() = %q, хочу пусто", got)
	}
	d.Write(Record{Call: 1}) // не паникует

	removed, err := d.Prune(time.Now(), Retention)
	if err != nil || removed != 0 {
		t.Fatalf("Prune() = %d, %v", removed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.RunPrune(ctx, time.Hour) // сразу выходит
}

// TestDumperPruneKeepsForeignFiles — ретенция трогает только *.json: чужие
// файлы и подкаталоги в каталоге дампа остаются.
func TestDumperPruneKeepsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	old := time.Now().Add(-Retention - time.Hour)
	files := map[string]bool{
		"turn-1-call-1.json": true, // старый дамп — удалить
		"README.txt":         false,
		"backup.json.bak":    false,
		"subdir":             false,
	}
	for name := range files {
		path := filepath.Join(dir, name)
		if name == "subdir" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("подкаталог не создан: %v", err)
			}
		} else if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("%s не создан: %v", name, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("время %s не подменено: %v", name, err)
		}
	}

	removed, err := d.Prune(time.Now(), Retention)
	if err != nil {
		t.Fatalf("ретенция упала: %v", err)
	}
	if removed != 1 {
		t.Fatalf("удалено %d файлов, хочу 1", removed)
	}
	for name := range files {
		_, statErr := os.Stat(filepath.Join(dir, name))
		if name == "turn-1-call-1.json" {
			if !os.IsNotExist(statErr) {
				t.Fatalf("старый дамп остался: %v", statErr)
			}
			continue
		}
		if statErr != nil {
			t.Fatalf("ретенция тронула чужое %q: %v", name, statErr)
		}
	}
}

// TestDumperPruneMissingDir — каталог исчез между стартом и ретенцией:
// ошибка возвращается, вызывающий логирует её, процесс живёт.
func TestDumperPruneMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("каталог не удалён: %v", err)
	}
	if _, err := d.Prune(time.Now(), Retention); err == nil {
		t.Fatal("Prune на исчезнувшем каталоге не вернул ошибку")
	}
}

// TestDumperRunPruneAtStart — ретенция срабатывает сразу при запуске, до
// первого тика; отменённый ctx завершает цикл.
func TestDumperRunPruneAtStart(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDumper(dir, func(string, ...any) {})
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	old := filepath.Join(dir, "bg-task-call-1.json")
	if err := os.WriteFile(old, []byte("{}"), 0o600); err != nil {
		t.Fatalf("старый дамп не создан: %v", err)
	}
	stale := time.Now().Add(-Retention - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("время не подменено: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.RunPrune(ctx, time.Hour)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("стартовый прогон ретенции не удалил старый дамп: %v", err)
	}
}

// TestDumperRunPruneNilLogger — ретенция с nil-логгером не паникует, когда
// действительно удаляет файлы: логирование — best-effort, как и запись.
func TestDumperRunPruneNilLogger(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	old := filepath.Join(dir, "turn-1-call-1.json")
	if err := os.WriteFile(old, []byte("{}"), 0o600); err != nil {
		t.Fatalf("старый дамп не создан: %v", err)
	}
	stale := time.Now().Add(-Retention - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("время не подменено: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ретенция с nil-логгером запаниковала: %v", r)
		}
	}()
	d.RunPrune(ctx, time.Hour)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("старый дамп не удалён: %v", err)
	}
}

// TestRecordFileName — имя файла различает ход и фон, санитизирует имя
// задачи и не даёт выйти из каталога дампа.
func TestRecordFileName(t *testing.T) {
	cases := []struct {
		rec  Record
		want string
	}{
		{Record{TurnID: 246, Call: 1}, "turn-246-call-1.json"},
		{Record{Task: "intention:42", Call: 2}, "bg-intention-42-call-2.json"},
		{Record{Task: "", Call: 3}, "bg-unknown-call-3.json"},
		{Record{TurnID: 7, Task: "и фоном", Call: 4}, "turn-7-call-4.json"},
	}
	for _, tc := range cases {
		if got := recordFileName(tc.rec); got != tc.want {
			t.Fatalf("recordFileName(%+v) = %q, хочу %q", tc.rec, got, tc.want)
		}
	}

	// Путь-обход в имени фоновой задачи не должен становиться путём.
	evil := recordFileName(Record{Task: "../../etc/passwd", Call: 1})
	if strings.ContainsAny(evil, `/\`) || strings.Contains(evil, "..") && strings.Contains(evil, "/") {
		t.Fatalf("имя файла содержит путь: %q", evil)
	}
	if filepath.Base(evil) != evil {
		t.Fatalf("имя файла не в каталоге дампа: %q", evil)
	}

	// Длинное имя задачи обрезается (имя файла не должно упираться в лимит ФС).
	long := recordFileName(Record{Task: strings.Repeat("a", 300), Call: 1})
	if len(long) > 120 {
		t.Fatalf("имя файла не обрезано: %d символов", len(long))
	}
}

// TestDumperWriteFailureIsBestEffort — каталог пропал после создания дампера:
// запись логируется, ошибка не выходит наружу (модельный вызов не падает).
func TestDumperWriteFailureIsBestEffort(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	var logs []string
	d, err := NewDumper(dir, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("каталог не удалён: %v", err)
	}
	d.Write(Record{TurnID: 1, Call: 1, Request: map[string]any{"model": "m"}})
	if len(logs) == 0 {
		t.Fatal("сбой записи дампа не залогирован")
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "не создан") {
		t.Fatalf("лог не объясняет сбой: %s", joined)
	}
}

// TestDumperWriteFailureNilLogger — дампер с nil-логгером (как в тестах и
// ручных вызовах) не должен паниковать, если запись не удалась: отладка не
// роняет процесс.
func TestDumperWriteFailureNilLogger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	d, err := NewDumper(dir, nil)
	if err != nil {
		t.Fatalf("дампер не создан: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("каталог не удалён: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("запись дампа с nil-логгером запаниковала: %v", r)
		}
	}()
	d.Write(Record{TurnID: 1, Call: 1})
}
