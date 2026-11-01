//go:build db

// Приёмочные БД-тесты пункта 16.3 (тестировщик, свежий контекст): сценарии ТЗ,
// не покрытые тестами разработчика, —
//   - ошибка записи MarkSent ПОСЛЕ подтверждённой отправки: исход неясен →
//     unknown, зависимые пузыри отменяются, вслепую не повторяется (§13.2);
//   - условность возврата из sending: терминальный исход не перезаписывается;
//   - проводка попыток: ошибка БД до отправки на исчерпанном лимите — failed,
//     а не вечный retry_wait.
package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// singleConnRepo — репозиторий на пуле из одного соединения без фоновых
// проверок здоровья: после pg_terminate_backend первый запрос падает на
// мёртвом соединении, следующий получает свежее (тот же приём, что в
// TestDeliveryLoopDBErrorBeforeSendRequeues).
func singleConnRepo(t *testing.T, pool *pgxpool.Pool) *OutboxRepo {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatalf("конфиг пула не разбирается: %v", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.HealthCheckPeriod = time.Hour
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("пул доставки не создан: %v", err)
	}
	t.Cleanup(single.Close)
	return NewOutboxRepo(single)
}

// killSingleConn обрывает единственное соединение пула доставки со стороны
// СУБД (pg_terminate_backend).
func killSingleConn(t *testing.T, pool *pgxpool.Pool, repo *OutboxRepo) {
	t.Helper()
	var backendPID int32
	if err := repo.Pool.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatalf("pid соединения не читается: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, backendPID); err != nil {
		t.Fatalf("соединение не оборвано: %v", err)
	}
}

// TestDeliveryDBErrorAfterSendMarksUnknown — 16.3(а): ошибка записи ПОСЛЕ
// подтверждённой отправки (MarkSent не записан) — исход неясен: unknown, не
// отправлять вслепую (§13.2). Ошибка MarkSent моделируется детерминированно
// триггером, блокирующим только перевод в sent: StatusOf до отправки и
// MarkUnknown после — проходят, отправка в Telegram проходит.
func TestDeliveryDBErrorAfterSendMarksUnknown(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())

	first := insertOutbox(t, pool, 200, 42, 0, `{"kind":"message","text":"отправлен, sent не записан"}`)
	second := insertOutbox(t, pool, 200, 42, 1, `{"kind":"message","text":"зависимый пузырь"}`)

	// Триггер блокирует только статус sent — ошибка БД ровно между
	// подтверждённой отправкой и записью исхода.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION dio_test_block_sent() RETURNS trigger AS $fn$
		BEGIN
			RAISE EXCEPTION 'sent заблокирован тестовым триггером (16.3)';
		END
		$fn$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("функция триггера не создана: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER dio_test_block_sent BEFORE UPDATE ON delivery_outbox
		FOR EACH ROW WHEN (NEW.status = 'sent') EXECUTE FUNCTION dio_test_block_sent()`); err != nil {
		t.Fatalf("триггер не создан: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER dio_test_block_sent ON delivery_outbox`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION dio_test_block_sent`)
	})

	loop := NewDeliveryLoop(pool, NewOutboxRepo(pool), client, nil,
		DeliveryOptions{BubbleGap: time.Millisecond})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	// Первый пузырь: физически отправлен, исход не записан → unknown с
	// причиной; зависимый — cancelled (§13.2: unknown не повторяется,
	// зависимые отменяются).
	var status, lastError string
	if err := pool.QueryRow(ctx,
		`SELECT status, coalesce(last_error, '') FROM delivery_outbox WHERE id = $1`, first).
		Scan(&status, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "unknown" {
		t.Fatalf("после ошибки записи sent строка в %q, хочу unknown (§13.2: не отправлять вслепую)", status)
	}
	if !strings.Contains(lastError, "sent не записан") {
		t.Fatalf("причина unknown не отражает «отправлено, но исход не записан»: %q", lastError)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, second).Scan(&status); err != nil {
		t.Fatalf("зависимый пузырь не читается: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("зависимый пузырь %q, хочу cancelled", status)
	}
	if sent := stub.sentMessages(); len(sent) != 1 || sent[0]["text"] != "отправлен, sent не записан" {
		t.Fatalf("ожидалась ровно одна физическая отправка: %v", sent)
	}

	// Не отправлять вслепую: повторные такты не трогают unknown/cancelled
	// (терминальны для ClaimDue) и не добавляют отправок.
	for i := 0; i < 3; i++ {
		if err := loop.Tick(ctx); err != nil {
			t.Fatalf("повторный такт %d не удался: %v", i, err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, first).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "unknown" {
		t.Fatalf("unknown не терминален: %q", status)
	}
	if sent := stub.sentMessages(); len(sent) != 1 {
		t.Fatalf("unknown отправляется вслепую повторно: %v", sent)
	}
}

// TestRequeueFromSendingKeepsTerminalStatus — 16.3(а): возврат из sending в
// retry_wait условен (как MarkSent): строку с уже случившимся терминальным
// исходом (cancelled) возврат не перезаписывает — ошибка без изменения
// статуса; строку в sending — возвращает.
func TestRequeueFromSendingKeepsTerminalStatus(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	sending := insertOutbox(t, pool, 210, 42, 0, `{"kind":"message","text":"вернётся в очередь"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'sending', attempts = 1 WHERE id = $1`, sending); err != nil {
		t.Fatalf("sending не выставлен: %v", err)
	}
	notBefore := time.Now().Add(time.Minute)
	if err := repo.RequeueFromSending(ctx, sending, notBefore, "ошибка БД до отправки: тест"); err != nil {
		t.Fatalf("возврат из sending не удался: %v", err)
	}
	var status string
	var gotNotBefore time.Time
	if err := pool.QueryRow(ctx,
		`SELECT status, not_before FROM delivery_outbox WHERE id = $1`, sending).Scan(&status, &gotNotBefore); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "retry_wait" {
		t.Fatalf("строка в %q, хочу retry_wait", status)
	}
	if gotNotBefore.IsZero() {
		t.Fatal("пауза повтора (not_before) не записана")
	}

	terminal := insertOutbox(t, pool, 210, 42, 1, `{"kind":"message","text":"терминальная"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'cancelled', last_error = 'отменено' WHERE id = $1`, terminal); err != nil {
		t.Fatalf("cancelled не выставлен: %v", err)
	}
	if err := repo.RequeueFromSending(ctx, terminal, notBefore, "не должно записаться"); err == nil {
		t.Fatal("возврат терминальной строки обязан ошибиться (условная запись)")
	}
	var termStatus, termError string
	if err := pool.QueryRow(ctx,
		`SELECT status, coalesce(last_error, '') FROM delivery_outbox WHERE id = $1`, terminal).
		Scan(&termStatus, &termError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if termStatus != "cancelled" || termError != "отменено" {
		t.Fatalf("терминальный исход перезаписан: status=%q last_error=%q", termStatus, termError)
	}
}

// TestDeliveryDBErrorBeforeSendGivesUpAfterAttempts — 16.3(а): ошибка БД до
// отправки считается попыткой: на исчерпанном лимите (attempts=MaxAttempts
// после захвата) строка уходит в failed, а не в вечный retry_wait.
func TestDeliveryDBErrorBeforeSendGivesUpAfterAttempts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())

	id := insertOutbox(t, pool, 220, 42, 0, `{"kind":"message","text":"предел попыток"}`)
	// attempts=4: захват поднимет до 5 = MaxAttempts — следующая ошибка БД
	// до отправки исчерпывает лимит.
	if _, err := pool.Exec(ctx, `UPDATE delivery_outbox SET attempts = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("attempts не выставлен: %v", err)
	}
	repo := singleConnRepo(t, pool)
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 1 {
		t.Fatalf("захвачено %d строк, хочу 1", len(claimed))
	}
	if claimed[0].Attempts != 5 {
		t.Fatalf("attempts после захвата = %d, хочу 5", claimed[0].Attempts)
	}
	killSingleConn(t, pool, repo)

	loop := NewDeliveryLoop(pool, repo, client, nil,
		DeliveryOptions{MaxAttempts: 5, RetryBase: time.Millisecond})
	loop.deliver(ctx, claimed[0])

	var status, lastError string
	if err := pool.QueryRow(ctx,
		`SELECT status, coalesce(last_error, '') FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "failed" {
		t.Fatalf("после исчерпания попыток строка в %q, хочу failed (иначе вечный флап одной строки)", status)
	}
	if !strings.Contains(lastError, "ошибка БД до отправки") {
		t.Fatalf("причина failed не отражает ошибку БД до отправки: %q", lastError)
	}

	// failed терминален: свежий такт не берёт и не отправляет.
	fresh := NewDeliveryLoop(pool, NewOutboxRepo(pool), client, nil, DeliveryOptions{})
	if err := fresh.Tick(ctx); err != nil {
		t.Fatalf("повторный такт не удался: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "failed" {
		t.Fatalf("failed не терминален: %q", status)
	}
	if sent := stub.sentMessages(); len(sent) != 0 {
		t.Fatalf("ничего не должно было уйти в Telegram: %v", sent)
	}
}
