//go:build db

// E2E устойчивости (§21.1, строка за строкой):
//   - сбой после model_output → результат применяется без повторной генерации;
//   - «SIGKILL»: обрыв соединения посреди генерации + перечитывание журнала
//     восстанавливает состояние (тот же stream, однократность применения);
//   - повторный запуск применённого хода не дублирует дельты;
//   - unknown-доставка не повторяется вслепую (§13.2);
//   - потеря singleton-соединения прекращает новые действия (§5.1).
//
// Сбой после model_output моделируется триггером БД с последовательностью
// (nextval не откатывается транзакцией): первая вставка памяти падает,
// вторая проходит — ровно одна платная генерация, восстановление по журналу.
package app

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
)

const r1Chat = int64(200601)

// r1TurnFinal — ответ хода: сообщение + кандидат памяти (через него срабатывает
// одноразовый сбой применения) + дельта настроения.
const r1TurnFinal = `{"decision":"answer","messages":["нашёл — держи"],` +
	`"memory":[{"kind":"claim","text":"Коля держит слово про тихий час","salience":0.6}],` +
	`"mood_delta":{"valence":0.1,"reason":"приятная договорённость"},"reason":"ответ"}`

// armApplyFailureOnce ставит триггер на memory_items: первая вставка после
// вооружения падает (RAISE), следующие проходят. nextval не откатывается
// вместе с транзакцией применения — «сбой ровно один раз» детерминирован.
func armApplyFailureOnce(t *testing.T, dsn string) {
	t.Helper()
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("admin-соединение не открыто: %v", err)
	}
	defer admin.Close(context.Background())
	stmts := []string{
		`CREATE SEQUENCE IF NOT EXISTS app_r1_fail_once_seq`,
		`CREATE OR REPLACE FUNCTION app_r1_fail_once() RETURNS trigger AS $$
		 BEGIN
		   IF nextval('app_r1_fail_once_seq')::int = 1 THEN
		     RAISE EXCEPTION 'искусственный сбой применения (тест 9.7)';
		   END IF;
		   RETURN NEW;
		 END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER app_r1_fail_once_insert BEFORE INSERT ON memory_items
		 FOR EACH ROW EXECUTE FUNCTION app_r1_fail_once()`,
	}
	for _, stmt := range stmts {
		if _, err := admin.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("триггер сбоя не установлен (%q): %v", stmt, err)
		}
	}
}

// blockingEmbedder — векторизатор, зависающий на первом вызове (имитация
// зависшего внешнего вызова посреди генерации); отпускается закрытием канала.
type blockingEmbedder struct {
	release chan struct{}
}

func (b *blockingEmbedder) Model() string { return "blocking-embedder" }

func (b *blockingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	<-b.release // внешняя сеть зависла: контекст не спасает (параметр_executor ago ждёт функцию)
	return [][]float32{make([]float32, 8)}, nil
}

var _ memory.Embedder = (*blockingEmbedder)(nil)

// journalEventCount — сколько событий данного типа в потоке хода (§12.1).
func journalEventCount(h *harness, turnID int64, eventType string) int64 {
	return queryInt(h, `
		SELECT count(*) FROM event_streams
		WHERE stream_id = $1 AND type = $2`, character.TurnStreamID(turnID), eventType)
}

// waitJournalEvent — ждёт появления события типа в потоке хода.
func waitJournalEvent(h *harness, what string, turnID int64, eventType string) {
	eventually(h.t, what, func() bool {
		return journalEventCount(h, turnID, eventType) > 0
	})
}

// waitForTurn ждёт создания хода в очереди и возвращает его id.
func waitForTurn(t *testing.T, h *harness, chatID int64) int64 {
	t.Helper()
	eventually(t, "ход создан буфером", func() bool {
		return queryInt(h, `SELECT count(*) FROM turns WHERE chat_id = $1`, chatID) > 0
	})
	return queryInt(h, `SELECT id FROM turns WHERE chat_id = $1 ORDER BY id LIMIT 1`, chatID)
}

// inputOf — вход повторного прогона хода тем же исполнителем (batch не нужен:
// восстановление применяет сохранённый результат).
func inputOf(turnID, chatID int64) character.Input {
	return character.Input{
		TurnID:  turnID,
		ChatID:  chatID,
		ScopeID: memory.ChatScope(fmt.Sprintf("%d", chatID)),
		Direct:  true,
		Attempt: 99,
	}
}

// TestResilienceRecoverAfterModelOutput — §21.1 «сбой после result_ready»:
// применение падает после записи model_output; повторный прогон применяет
// сохранённый результат БЕЗ новой генерации, дельты и память — однократно.
func TestResilienceRecoverAfterModelOutput(t *testing.T) {
	dsn := newTestDB(t)
	h := newHarnessDSN(t, dsn, textResp(r1TurnFinal))
	armApplyFailureOnce(t, dsn) // после миграций: таблицы уже есть

	h.api.push(1, r1Chat, "private", "я пообещал себе тихий час")

	// Восстановительный прогон применил сохранённый результат: сообщение
	// доставлено, ход применён.
	eventually(t, "сообщение доставлено после сбоя применения", func() bool {
		return len(h.api.callsOf("sendMessage")) == 1
	})

	turnID := queryInt(h, `SELECT id FROM turns WHERE chat_id = $1`, r1Chat)
	if status := queryString(h, `SELECT status FROM turns WHERE id = $1`, turnID); status != "applied" {
		t.Fatalf("ход не применён: %s", status)
	}

	// БЕЗ новой генерации: ровно один модельный вызов на весь сценарий.
	if got := h.modelCalls(); got != 1 {
		t.Fatalf("модельных вызовов %d, хочу 1 — восстановление повторило генерацию", got)
	}
	// Журнал: model_output один, deltas_applied один (однократность §12.1).
	if n := journalEventCount(h, turnID, "model_output"); n != 1 {
		t.Fatalf("model_output в журнале %d, хочу 1", n)
	}
	if n := journalEventCount(h, turnID, "deltas_applied"); n != 1 {
		t.Fatalf("deltas_applied в журнале %d, хочу 1", n)
	}
	if n := queryInt(h, `SELECT count(*) FROM memory_items`); n != 1 {
		t.Fatalf("записей памяти %d, хочу 1 — дубль применения", n)
	}
	if n := queryInt(h, `SELECT count(*) FROM delivery_outbox`); n != 1 {
		t.Fatalf("строк outbox %d, хочу 1", n)
	}
}

// TestResiliencePollingDuringLLMHang — §21.1 строка 1 «LLM завис на timeout →
// Polling продолжает сохранять updates» (инвариант 1: ingest не ждёт модели).
// Зависание — на внешнем вызове инструмента памяти посреди генерации; пока ход
// висит, polling должен сохранять новые события и закрывать их в новые ходы.
func TestResiliencePollingDuringLLMHang(t *testing.T) {
	dsn := newTestDB(t)
	block := &blockingEmbedder{release: make(chan struct{})}
	h := newHarnessDSNTuned(t, dsn, func(o *Options) { o.Embedder = block },
		toolResp("c-h1", "memory_search", `{"query":"зависание"}`),
		textResp(`{"decision":"answer","messages":["ответ первого хода"],"reason":"ответ"}`),
		textResp(`{"decision":"answer","messages":["ответ второго хода"],"reason":"ответ"}`),
		textResp(`{"decision":"answer","messages":["ответ третьего хода"],"reason":"ответ"}`),
	)

	// Ход завис на внешнем вызове.
	h.api.push(1, r1Chat, "private", "первый вопрос — модель зависла")
	turn1 := waitForTurn(t, h, r1Chat)
	waitJournalEvent(h, "ход ушёл в генерацию", turn1, "tool_call")

	// Пока LLM завис: polling продолжает сохранять updates (§21.1) — событие
	// второе сохраняется и буфер закрывает его в новый ход.
	h.api.push(2, r1Chat, "private", "второе сообщение во время зависания")
	eventually(t, "второй ход создан, пока модель зависла", func() bool {
		return queryInt(h, `SELECT count(*) FROM turns WHERE chat_id = $1`, r1Chat) >= 2
	})
	// ...и третье — тоже (polling не остановился после первого).
	h.api.push(3, r1Chat, "private", "третье сообщение во время зависания")
	eventually(t, "третий ход создан, пока модель зависла", func() bool {
		return queryInt(h, `SELECT count(*) FROM turns WHERE chat_id = $1`, r1Chat) >= 3
	})
	if n := queryInt(h, `SELECT count(*) FROM social_events WHERE chat_id = $1`, r1Chat); n != 3 {
		t.Fatalf("событий сохранено %d, хочу 3 — polling терял updates при зависшей модели", n)
	}
	// Зависший ход ничего не отправил (применения не было).
	if n := len(h.api.callsOf("sendMessage")); n != 0 {
		t.Fatalf("зависший ход отправил %d сообщений, хочу 0", n)
	}

	// Зависание отпущено: ход дорабатывает, очередь разгребается по порядку.
	close(block.release)
	eventually(t, "все три хода доставлены после отпуска", func() bool {
		return len(h.api.callsOf("sendMessage")) == 3
	})
	if got := h.modelCalls(); got != 4 {
		t.Fatalf("модельных вызовов %d, хочу 4 (инструментный раунд + три ответа)", got)
	}
}

// TestResilienceSigkillMidTurn — §12.2/§21.1 «SIGKILL»: соединение оборвано
// посреди генерации; перезапуск восстанавливает состояние перечитыванием
// журнала (тот же stream_id) и доводит ход без дублей.
func TestResilienceSigkillMidTurn(t *testing.T) {
	dsn := newTestDB(t)
	block := &blockingEmbedder{release: make(chan struct{})}

	// Процесс A: генерация доходит до инструмента памяти и зависает в нём.
	hA := newHarnessDSNTuned(t, dsn, func(o *Options) { o.Embedder = block },
		toolResp("c-a1", "memory_search", `{"query":"винил"}`),
		textResp(`{"decision":"answer","messages":["ответ процесса A"],"reason":"ответ"}`),
	)
	hA.api.push(1, r1Chat, "private", "проверка SIGKILL")

	// Посреди генерации: ход создан, инструмент вызван (обращение зафиксировано
	// журналом §12.1) — и «SIGKILL»: контекст отменён, соединения закрыты.
	turnID := waitForTurn(t, hA, r1Chat)
	waitJournalEvent(hA, "tool_call посреди генерации", turnID, "tool_call")
	hA.hardKill()

	// Процесс B: повтор хода ограниченно с тем же stream_id (§12.2).
	hB := newHarnessDSN(t, dsn,
		toolResp("c-b1", "memory_search", `{"query":"винил"}`),
		textResp(`{"decision":"answer","messages":["восстановлено после SIGKILL"],"reason":"ответ"}`),
	)
	eventually(t, "процесс B доставил ответ восстановленного хода", func() bool {
		return len(hB.api.callsOf("sendMessage")) == 1
	})

	// Состояние восстановлено перечитыванием журнала, а не повторным
	// исполнением решений: turn_started один на два процесса...
	if n := journalEventCount(hB, turnID, "turn_started"); n != 1 {
		t.Fatalf("turn_started в журнале %d, хочу 1 — поток хода раздвоился", n)
	}
	// ...применение однократно, outbox один...
	if n := journalEventCount(hB, turnID, "deltas_applied"); n != 1 {
		t.Fatalf("deltas_applied в журнале %d, хочу 1", n)
	}
	if n := queryInt(hB, `SELECT count(*) FROM delivery_outbox`); n != 1 {
		t.Fatalf("строк outbox %d, хочу 1", n)
	}
	// ...и ровно два модельных вызова в B (инструментный раунд + финал).
	if got := hB.modelCalls(); got != 2 {
		t.Fatalf("модельных вызовов в B %d, хочу 2", got)
	}

	// Отпускаем зависший вызов процесса A и ждём, пока его worker реально
	// остановится (Done закрывается после возврата из Run): продолжение A
	// падает на закрытом пуле и ничего не портит (CAS хода уже у B).
	close(block.release)
	select {
	case <-hA.app.cognit.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("worker процесса A не остановился после release")
	}
	if n := queryInt(hB, `SELECT count(*) FROM delivery_outbox`); n != 1 {
		t.Fatalf("после освобождения A строк outbox %d, хочу 1 — дубль доставки", n)
	}
}

// TestResilienceDoubleRunAppliedTurn — §21.1 «повторный запуск applied turn»:
// mood, relationships и memories не дублируются.
func TestResilienceDoubleRunAppliedTurn(t *testing.T) {
	h := newHarness(t,
		textResp(`{"decision":"answer","messages":["раз"],`+
			`"memory":[{"kind":"preference","text":"Коля ценит тишину по утрам","salience":0.5}],`+
			`"mood_delta":{"valence":0.2,"reason":"спокойная тема"},"reason":"ответ"}`),
	)
	h.api.push(1, r1Chat, "private", "утром я люблю тишину")
	eventually(t, "ход применён и доставлен", func() bool {
		return len(h.api.callsOf("sendMessage")) == 1
	})

	turnID := queryInt(h, `SELECT id FROM turns WHERE chat_id = $1`, r1Chat)
	moodBefore := queryString(h, `SELECT valence::text FROM character_state WHERE id = 1`)
	memBefore := queryInt(h, `SELECT count(*) FROM memory_items`)
	outboxBefore := queryInt(h, `SELECT count(*) FROM delivery_outbox`)
	relBefore := queryInt(h, `SELECT count(*) FROM relationships`)

	// Повторный запуск применённого хода тем же исполнителем (окно «commit
	// прошёл, а вызывающий видел сбой» — §12.1): журнал отвечает AlreadyApplied.
	out, err := h.app.runner.Run(context.Background(), inputOf(turnID, r1Chat))
	if err != nil {
		t.Fatalf("повторный прогон упал: %v", err)
	}
	if !out.AlreadyApplied {
		t.Fatalf("повторный прогон не распознан: %+v", out)
	}
	if got := h.modelCalls(); got != 1 {
		t.Fatalf("повторный прогон сделал %d модельных вызовов, хочу 0 новых", got-1)
	}

	if moodAfter := queryString(h, `SELECT valence::text FROM character_state WHERE id = 1`); moodAfter != moodBefore {
		t.Fatalf("mood изменился повтором: %s -> %s", moodBefore, moodAfter)
	}
	if got := queryInt(h, `SELECT count(*) FROM memory_items`); got != memBefore {
		t.Fatalf("память задублировалась: %d -> %d", memBefore, got)
	}
	if got := queryInt(h, `SELECT count(*) FROM delivery_outbox`); got != outboxBefore {
		t.Fatalf("outbox задублировался: %d -> %d", outboxBefore, got)
	}
	if got := queryInt(h, `SELECT count(*) FROM relationships`); got != relBefore {
		t.Fatalf("отношения задублировались: %d -> %d", relBefore, got)
	}
}

// TestResilienceUnknownDelivery — §13.2/§21.1 «unknown без слепого resend»:
// обрыв без ответа переводит доставку в unknown, зависимые отменяются,
// повторные такты не отправляют повторно, свежая доставка работает.
func TestResilienceUnknownDelivery(t *testing.T) {
	h := newHarness(t,
		textResp(`{"decision":"answer","messages":["первый пузырь","второй пузырь"],"reason":"ответ"}`),
		textResp(`{"decision":"answer","messages":["свежий ответ"],"reason":"ответ на новый ход"}`),
	)

	attempts := atomic.Int64{}
	h.api.onSendMessage = func(chatID int64, text string) (int64, error) {
		n := attempts.Add(1)
		if n == 1 {
			return 0, io.ErrUnexpectedEOF // ответ не получен: исход неизвестен
		}
		return int64(3000 + n), nil
	}

	h.api.push(1, r1Chat, "private", "напиши что-нибудь")
	turnID := waitForTurn(t, h, r1Chat)
	// Первый bubble: отправка неопределённа → unknown; зависимый — cancelled.
	eventually(t, "доставка переведена в unknown с отменой зависимого", func() bool {
		first, ok := queryStringOK(h, `SELECT status FROM delivery_outbox WHERE turn_id = $1 ORDER BY position LIMIT 1`, turnID)
		if !ok {
			return false // применение хода ещё не создало строки outbox
		}
		second, _ := queryStringOK(h, `SELECT status FROM delivery_outbox WHERE turn_id = $1 ORDER BY position OFFSET 1 LIMIT 1`, turnID)
		return first == "unknown" && second == "cancelled"
	})
	if errText := queryString(h, `SELECT coalesce(last_error,'') FROM delivery_outbox WHERE turn_id = $1 ORDER BY position LIMIT 1`, turnID); errText == "" {
		t.Fatalf("unknown без причины в last_error")
	}

	// Слепого resend нет и свежая доставка работает — одним условием, без
	// пауз: ждём, пока новый ход действительно доставлен (строка sent). Такт
	// доставки берёт строки в порядке (чат, ход, позиция): если бы unknown
	// переотправлялся, он был бы захвачен в том же такте раньше свежей строки,
	// и итоговый счётчик попыток был бы больше двух.
	h.api.push(2, r1Chat, "private", "и ещё раз напиши")
	eventually(t, "свежая доставка прошла", func() bool {
		return queryInt(h, `SELECT count(*) FROM delivery_outbox WHERE turn_id <> $1 AND status = 'sent'`, turnID) == 1
	})
	if got := attempts.Load(); got != 2 {
		t.Fatalf("sendMessage вызван %d раз, хочу 2 (unknown не повторяют, свежая доставка — один раз)", got)
	}
}

// TestResilienceSingletonConnectionLost — §5.1/§21.1 «потеря singleton
// connection»: процесс прекращает новые действия; работа подхватывается
// перезапуском (модель systemd restart).
func TestResilienceSingletonConnectionLost(t *testing.T) {
	dsn := newTestDB(t)
	hA := newHarnessDSN(t, dsn,
		textResp(`{"decision":"answer","messages":["ответ живого процесса"],"reason":"ответ"}`),
		textResp(`{"decision":"answer","messages":["ответ после перезапуска"],"reason":"ответ"}`),
	)
	hA.api.push(1, r1Chat, "private", "привет до потери соединения")
	eventually(t, "первый ход доставлен", func() bool {
		return len(hA.api.callsOf("sendMessage")) == 1
	})

	// Обрыв singleton-соединения со стороны СУБД (как при сетевом сбое).
	if _, err := hA.pool.Exec(context.Background(),
		`SELECT pg_terminate_backend($1)`, hA.app.single.PID()); err != nil {
		t.Fatalf("singleton-соединение не оборвано: %v", err)
	}

	// Процесс замечает потерю и прекращает новые действия (§5.1).
	eventually(t, "процесс зафиксировал потерю singleton", func() bool {
		hA.app.mu.Lock()
		defer hA.app.mu.Unlock()
		return hA.app.fatal != nil
	})

	// Новое событие не обрабатывается мёртвым процессом — и не теряется:
	// его возьмёт следующий процесс с сохранённого offset (инвариант 3).
	hA.api.push(2, r1Chat, "private", "привет после потери")
	time.Sleep(300 * time.Millisecond)
	if n := queryInt(hA, `SELECT count(*) FROM social_events`); n != 1 {
		t.Fatalf("мёртвый процесс обработал новое событие: событий %d, хочу 1", n)
	}
	if n := len(hA.api.callsOf("sendMessage")); n != 1 {
		t.Fatalf("мёртвый процесс отправил сообщение: %d вызовов, хочу 1", n)
	}

	// Перезапуск: singleton свободен (замок умер вместе с соединением),
	// новый процесс подхватывает оставшееся событие. Telegram — внешний мир:
	// очередь updates на его стороне общая для обоих процессов, offset — в БД.
	hB := newHarnessDSNTuned(t, dsn, func(o *Options) { o.Telegram = hA.api },
		textResp(`{"decision":"answer","messages":["ответ после перезапуска"],"reason":"ответ"}`),
	)
	hB.api = hA.api
	eventually(t, "перезапуск доставил отложенное", func() bool {
		return len(hB.api.callsOf("sendMessage")) == 2
	})
	if got := hB.api.callsOf("sendMessage")[1].Text; got != "ответ после перезапуска" {
		t.Fatalf("после перезапуска доставлено не то: %q", got)
	}
}
