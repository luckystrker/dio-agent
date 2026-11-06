// Запись дампа модельных вызовов (40.1): файл на вызов, права 0600, каталог
// 0700, ретенция 3 дня (удаление при старте и раз в сутки).
package modebug

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Retention — сколько дней живут файлы дампа. Отладка «каши» в ответах не
// должна копиться на диске VPS вечно (этап 40.1).
const Retention = 3 * 24 * time.Hour

// Record — один модельный вызов как он был на проводе. Request и Response —
// сырые тела (json.RawMessage — байт-в-байт, не пересобранные); для
// стрим-ответа Response — объект со склеенным текстом/reasoning и сырыми
// чанками (ограниченно). Ключ API в файл не попадает никогда.
type Record struct {
	// TurnID — номер разговорного хода (0 — фоновая задача).
	TurnID int64 `json:"turn_id,omitempty"`
	// Task — имя фоновой задачи (пусто у хода).
	Task string `json:"task,omitempty"`
	// Call — номер HTTP-вызова внутри хода/задачи, с 1.
	Call int `json:"call"`
	// At — момент начала вызова.
	At time.Time `json:"at"`
	// URL — адрес, на который ушёл запрос (нужен replay: 40.2).
	URL string `json:"url,omitempty"`
	// Headers — заголовки запроса без Authorization (x-opencode-session можно).
	Headers map[string]string `json:"headers,omitempty"`
	// Request — тело запроса как ушло провайдеру.
	Request any `json:"request"`
	// Response — тело ответа как пришло (сырое или свёртка стрима).
	Response any `json:"response"`
	// Status — HTTP-код (0 — транспортная ошибка).
	Status int `json:"status"`
	// LatencyMs — сколько занял вызов, от запроса до конца тела.
	LatencyMs int64 `json:"latency_ms"`
	// Error — текст транспортной ошибки (пусто — ошибки не было).
	Error string `json:"error,omitempty"`
}

// Dumper пишет дампы в каталог. nil-указатель — дамп выключен (MODEL_DEBUG_DUMP_DIR
// пуст): все методы безопасны на nil.
type Dumper struct {
	dir  string
	logf func(format string, args ...any)
}

// NewDumper создаёт каталог дампа (0700). Пустой путь — (nil, nil): дамп
// выключен. Ошибка создания — причина не запускать дамп, но не процесс:
// отладочная функция не должна валить живой сервис (решение принимает app).
func NewDumper(dir string, logf func(format string, args ...any)) (*Dumper, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("каталог дампа модели %q не создан: %w", dir, err)
	}
	// Уже существующий каталог MkdirAll не ужесточает: дамп приватный, права
	// приводятся к 0700 явно (файлы всё равно 0600). Смена прав существующего
	// каталога — в лог: оператор должен видеть, что права поменялись.
	oldPerm := os.FileMode(0)
	if info, serr := os.Stat(dir); serr == nil {
		oldPerm = info.Mode().Perm()
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("каталог дампа модели %q: права 0700 не выставлены: %w", dir, err)
	}
	if logf != nil && oldPerm != 0 && oldPerm != 0o700 {
		logf("дамп модели: права каталога %s %o → 700 (дамп приватный)", dir, oldPerm)
	}
	return &Dumper{dir: dir, logf: logf}, nil
}

// log — best-effort журнал дампера: nil-логгер молчит, а не паникует
// (NewDumper(dir, nil) разрешён и используется в тестах).
func (d *Dumper) log(format string, args ...any) {
	if d == nil || d.logf == nil {
		return
	}
	d.logf(format, args...)
}

// Dir — каталог дампа (пусто — выключен).
func (d *Dumper) Dir() string {
	if d == nil {
		return ""
	}
	return d.dir
}

// Write сохраняет запись best-effort: сбой дампа не роняет модельный вызов.
func (d *Dumper) Write(rec Record) {
	if d == nil {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		d.log("дамп модели: запись не сериализуется (call %d): %v", rec.Call, err)
		return
	}
	name := filepath.Join(d.dir, recordFileName(rec))
	// Атомарная запись: temp + rename — падение процесса не оставит битый JSON
	// на месте готового дампа (временный файл ретенция удалит как чужой).
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		d.log("дамп модели: файл %s не создан: %v", tmp, err)
		return
	}
	fail := func(format string, args ...any) {
		d.log(format, args...)
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, werr := f.Write(raw); werr != nil {
		fail("дамп модели: файл %s не записан: %v", tmp, werr)
		return
	}
	if cerr := f.Chmod(0o600); cerr != nil {
		fail("дамп модели: права %s не выставлены: %v", tmp, cerr)
		return
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		d.log("дамп модели: файл %s не закрыт: %v", tmp, cerr)
		return
	}
	if rerr := os.Rename(tmp, name); rerr != nil {
		_ = os.Remove(tmp)
		d.log("дамп модели: файл %s не переименован: %v", name, rerr)
	}
}

// recordFileName — turn-<id>-call-<n>.json / bg-<task>-call-<n>.json.
func recordFileName(rec Record) string {
	prefix := "unknown"
	switch {
	case rec.TurnID > 0:
		prefix = fmt.Sprintf("turn-%d", rec.TurnID)
	case rec.Task != "":
		prefix = "bg-" + sanitizeName(rec.Task)
	}
	return fmt.Sprintf("%s-call-%d.json", prefix, rec.Call)
}

// sanitizeName приводит имя задачи к безопасному виду файла: только буквы,
// цифры, точка, подчёркивание и дефис; остальное — дефис.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 80 {
		out = out[:80]
	}
	if out == "" {
		return "task"
	}
	return out
}

// Prune удаляет файлы дампа старше maxAge. Возвращает число удалённых.
func (d *Dumper) Prune(now time.Time, maxAge time.Duration) (int, error) {
	if d == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return 0, fmt.Errorf("дамп модели: каталог %q не читается: %w", d.dir, err)
	}
	cutoff := now.Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !isDumpFileName(e.Name()) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if rerr := os.Remove(filepath.Join(d.dir, e.Name())); rerr != nil {
				return removed, fmt.Errorf("дамп модели: %s не удалён: %w", e.Name(), rerr)
			}
			removed++
		}
	}
	return removed, nil
}

// isDumpFileName — только файлы дампа (turn-<id>-call-<n>.json /
// bg-<task>-call-<n>.json): ретенция не имеет права сносить чужие JSON,
// если MODEL_DEBUG_DUMP_DIR по ошибке указал на чужой каталог.
func isDumpFileName(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	base := strings.TrimSuffix(name, ".json")
	if !strings.HasPrefix(base, "turn-") && !strings.HasPrefix(base, "bg-") {
		return false
	}
	idx := strings.LastIndex(base, "-call-")
	if idx < 0 {
		return false
	}
	n := base[idx+len("-call-"):]
	if n == "" {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// RunPrune чистит дамп при старте и затем раз в сутки до отмены ctx.
func (d *Dumper) RunPrune(ctx context.Context, interval time.Duration) {
	if d == nil {
		return
	}
	prune := func() {
		n, err := d.Prune(time.Now(), Retention)
		if err != nil {
			d.log("дамп модели: ретенция: %v", err)
			return
		}
		if n > 0 {
			d.log("дамп модели: удалено %d файлов старше %d дней", n, int(Retention.Hours()/24))
		}
	}
	prune()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
