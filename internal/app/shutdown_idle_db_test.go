//go:build db

// Доп-проверка 18.4 к TestShutdownDrainsWorkerBeforeFlush: частый прод-случай
// стопа — worker ПРОСТАИВАЕТ (никакого текущего хода), но буфер чата жив.
// systemctl stop в такой момент не должен запускать модельный ход вовсе:
// счётчик вызовов скриптовой модели = 0, порция закрыта в ход created для
// следующего запуска, событие не потеряно.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestShutdownIdleWorkerStartsNoModelTurn(t *testing.T) {
	const groupChat = int64(-100777)
	dsn := newTestDB(t)
	h := newHarnessDSNTuned(t, dsn, func(o *Options) {
		// Длинные окна: порция группы живёт до FlushAll, не закрывается таймером.
		o.Buffers = func(ct telegram.ChatType, _ telegram.EventKind) telegram.Windows {
			return telegram.Windows{Idle: 10 * time.Second, Max: 15 * time.Second}
		}
	}, textResp(lcAnswer)) // ответ есть в скрипте: если ход начнётся, счётчик это увидит

	// Упоминание в группе → pending (напрямую в очередь заглушки Bot API —
	// нужен entities-массив упоминания). Хода нет: очередь пуста, worker простаивает.
	h.api.mu.Lock()
	h.api.updates = append(h.api.updates, telegram.Update{UpdateID: 2, Raw: json.RawMessage(fmt.Sprintf(
		`{"update_id":2,"message":{"message_id":2,"date":%d,"text":"@dio_bot глянь",`+
			`"from":{"id":777,"first_name":"Коля"},"chat":{"id":%d,"type":"supergroup"},`+
			`"entities":[{"type":"mention","offset":0,"length":8}]}}`, time.Now().Unix(), groupChat))})
	h.api.mu.Unlock()
	eventually(t, "событие группы сохранено (pending)", func() bool {
		return queryInt(h, `SELECT count(*) FROM social_events WHERE chat_id = $1 AND status = 'pending'`, groupChat) > 0
	})
	if n := queryInt(h, `SELECT count(*) FROM turns`); n != 0 {
		t.Fatalf("ход создан до Stop — тест не в состоянии «живой буфер, простаивающий worker»: %d", n)
	}

	done := make(chan struct{})
	go func() {
		h.app.Stop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("Stop не вернулся")
	}

	if calls := h.modelCalls(); calls != 0 {
		t.Fatalf("модельных вызовов при остановке %d, хочу 0 — Stop не должен стартовать модельный ход из живого буфера (18.4)", calls)
	}
	if n := queryInt(h, `SELECT count(*) FROM turns WHERE chat_id = $1`, groupChat); n != 1 {
		t.Fatalf("ход из буфера не создан при остановке: %d (событие теряет очередь)", n)
	}
	if status := queryString(h, `SELECT status FROM turns WHERE chat_id = $1`, groupChat); status != "created" {
		t.Fatalf("ход группы должен остаться created для следующего запуска: %s", status)
	}
	if n := queryInt(h, `SELECT count(*) FROM social_events WHERE chat_id = $1 AND status = 'buffered' AND turn_id IS NOT NULL`, groupChat); n != 1 {
		t.Fatalf("событие группы не закрыто в ход при остановке: %d", n)
	}
}
