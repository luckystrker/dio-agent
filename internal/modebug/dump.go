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
	return &Dumper{dir: dir, logf: logf}, nil
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
		d.logf("дамп модели: запись не сериализуется (call %d): %v", rec.Call, err)
		return
	}
	name := filepath.Join(d.dir, recordFileName(rec))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		d.logf("дамп модели: файл %s не создан: %v", name, err)
		return
	}
	if _, err := f.Write(raw); err != nil {
		d.logf("дамп модели: файл %s не записан: %v", name, err)
	}
	if err := f.Close(); err != nil {
		d.logf("дамп модели: файл %s не закрыт: %v", name, err)
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
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

// RunPrune чистит дамп при старте и затем раз в сутки до отмены ctx.
func (d *Dumper) RunPrune(ctx context.Context, interval time.Duration) {
	if d == nil {
		return
	}
	prune := func() {
		n, err := d.Prune(time.Now(), Retention)
		if err != nil {
			d.logf("дамп модели: ретенция: %v", err)
			return
		}
		if n > 0 {
			d.logf("дамп модели: удалено %d файлов старше %d дней", n, int(Retention.Hours()/24))
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
