//go:build db

// БД-тест дренаж-контекста адаптеров журнала/чекпоинтов (блок 17.1, §5.3/§12.2):
// SIGTERM отменяет сигнальный контекст ещё ДО graceful drain — записи журнала
// дренажа (model_output сгенерированного ответа, WAL, чекпоинты) обязаны жить.
// Адаптеры строятся на runCtx приложения, отменяемом в Stop ПОСЛЕ дренажа.
package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
)

// TestJournalLivesOnRunContextNotSignalCtx — запись журнала живёт после отмены
// внешнего сигнального ctx (весь дренаж) и падает после отмены runCtx.
func TestJournalLivesOnRunContextNotSignalCtx(t *testing.T) {
	dsn := newTestDB(t)
	opts := bareOptions(dsn)
	// Watcher живости singleton в bareOptions пингует каждые 20 мс: под
	// параллельной нагрузкой тестового Postgres ping-таймаут ловил «потерю
	// замка» и отменял runCtx раньше отмены сигнального ctx — гонка теста,
	// не предмета. Живость singleton здесь не проверяется: интервал — за
	// пределами теста.
	opts.SingletonWatch = time.Hour
	a, err := New(opts)
	if err != nil {
		t.Fatalf("приложение не собрано: %v", err)
	}
	signalCtx, cancelSignal := context.WithCancel(context.Background())
	defer cancelSignal()
	if err := a.Start(signalCtx); err != nil {
		t.Fatalf("приложение не запущено: %v", err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })

	// «SIGTERM»: сигнальный контекст отменен, graceful drain (§5.3) ещё идёт.
	cancelSignal()
	ev := &character.TurnStarted{TurnID: 17_0101, ChatID: 1701, Kind: "conversation"}
	if err := a.journal.WriteTurnStarted(context.Background(), ev); err != nil {
		t.Fatalf("запись журнала после отмены сигнального ctx не удалась — дренаж теряет события хода (§5.3 «сохраняя доступные результаты»): %v", err)
	}

	// runCtx отменяется ПОСЛЕ дренажа — с его отменой журнал действительно глохнет.
	a.cancelRun()
	evAfter := &character.TurnStarted{TurnID: 17_0102, ChatID: 1701, Kind: "conversation"}
	if err := a.journal.WriteTurnStarted(context.Background(), evAfter); err == nil {
		t.Fatal("запись журнала после отмены runCtx должна падать: адаптер обязан жить на runCtx, а не на чём-то другом")
	}
}

// TestCheckpointStoreLivesOnRunContextNotSignalCtx — вторая половина 17.1
// (мутация М2 приёмки): чекпоинт-адаптер — отдельный от журнала объект со своим
// ctx-полем, и его проводка тоже обязана идти через runCtx. После отмены
// сигнального ctx (весь дренаж) запись чекпоинта живёт — прогресс фоновой
// задачи не теряется при рестарте (§12.2); после отмены runCtx — падает.
func TestCheckpointStoreLivesOnRunContextNotSignalCtx(t *testing.T) {
	dsn := newTestDB(t)
	opts := bareOptions(dsn)
	// Та же гонка ping-watchera вне предмета, что у теста журнала.
	opts.SingletonWatch = time.Hour
	a, err := New(opts)
	if err != nil {
		t.Fatalf("приложение не собрано: %v", err)
	}
	signalCtx, cancelSignal := context.WithCancel(context.Background())
	defer cancelSignal()
	if err := a.Start(signalCtx); err != nil {
		t.Fatalf("приложение не запущено: %v", err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })

	// «SIGTERM»: сигнальный контекст отменён, graceful drain ещё идёт —
	// чекпоинт дренажа (точка восстановления задачи §12.2) обязан записаться.
	cancelSignal()
	const taskID = "task:drain:checkpoint"
	if err := a.checkpoints.Write(taskID, []byte(`{"step":"generate","attempt":2}`)); err != nil {
		t.Fatalf("запись чекпоинта после отмены сигнального ctx не удалась — дренаж теряет прогресс задач (§12.2): %v", err)
	}
	state, err := a.checkpoints.Read(taskID)
	if err != nil {
		t.Fatalf("чекпоинт не перечитан: %v", err)
	}
	var step struct {
		Step string `json:"step"`
	}
	if jerr := json.Unmarshal(state, &step); jerr != nil || step.Step != "generate" {
		t.Fatalf("чекпоинт вернул не то, что записан: %s (err=%v)", state, jerr)
	}

	// runCtx отменяется ПОСЛЕ дренажа — с его отменой чекпоинты глохнут.
	a.cancelRun()
	if err := a.checkpoints.Write(taskID, []byte(`{"step":"apply"}`)); err == nil {
		t.Fatal("запись чекпоинта после отмены runCtx должна падать: адаптер обязан жить на runCtx")
	}
}
