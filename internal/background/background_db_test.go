//go:build db

// БД-тесты фона (этап 9.6, §14, §15, §10.2, §5.2, §12.2, §20):
//   - dedupe намерений и однократность claim;
//   - гейты актуальности (истёк срок, дубль, чат ушёл вперёд);
//   - проактивные caps §14.3 (2/сутки, 4 часа, группы выключены);
//   - возобновление задачи с чекпоинта/WAL без повторной генерации;
//   - перенос фона при прямом обращении (§5.2);
//   - e2e инициативы (outbox, CAS, бюджет пула background) и консолидации
//     (summary, summarized_at, stances, отсутствие самоподтверждения §10.3);
//   - queueStats/heartbeat §20.
package background

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
)

const exploreTurnJSON = `{"decision":"ignore",` +
	`"memory":[{"kind":"thought","text":"Винил — живая тема Коли, стоит вернуться к ней позже","entities":[{"name":"пластинки","kind":"topic"}],"salience":0.4}],` +
	`"reason":"размышление для себя, ничего не отправляю"}`

const initiativeTurnJSON = `{"decision":"answer",` +
	`"messages":["Кстати, вспомнил про твои пластинки — как там поиск идёт?"],` +
	`"memory":[{"kind":"preference","text":"Коля ищет редкий пресс пластинки","salience":0.5}],` +
	`"mood_delta":{"valence":0.2,"reason":"рад вернуться к приятной теме"}}`

func consolidationJSON(summary, stanceSummary string) string {
	return fmt.Sprintf(`{"summary":%q,"stances":[{"entity":"Коля","aspect":"general","summary":%q,"strength":0.6,"interest":0.7}]}`,
		summary, stanceSummary)
}

// TestIntentionDedupeAndClaim — dedupe (kind, topic, chat) при создании §14.1
// и однократность claim: повторный такт/процесс то же намерение не забирает.
func TestIntentionDedupeAndClaim(t *testing.T) {
	f, ctx := newFixture(t)

	id := f.createIntention(t, ctx, KindReplyLater, "Пластинки", -6, bgChatID)
	// Повтор той же идеи в другом регистре и с хвостовым пробелом — дубликат.
	tx := mustTx(t, ctx, f.pool)
	_, created, err := character.CreateIntentionTx(ctx, tx, character.IntentionParams{
		Kind: KindReplyLater, ChatID: bgChatID, ScopeID: bgScope,
		Topic: "пластинки ", Reason: "тест dedupe", DelayHours: -6,
	})
	// Дубликат ничего не пишет — транзакция только откатывается (утечка
	// открытой транзакции блокирует pool.Close в cleanup).
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("повторное создание не выполнено: %v", err)
	}
	if created {
		t.Fatal("дубликат (та же тема в другом регистре/с пробелом) должен отсекаться dedupe-ключом §14.1")
	}

	claimed, err := f.intentions.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("claim не удался: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id || claimed[0].Status != "running" {
		t.Fatalf("claim должен взять ровно одно намерение в running: %+v", claimed)
	}

	// Повторный такт (или второй процесс): то же намерение не берётся.
	again, err := f.intentions.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("повторный claim не удался: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("повторный claim забрал уже исполняемое: %+v", again)
	}
}

// TestIntentionGates — гейты актуальности перед запуском (§14.1).
func TestIntentionGates(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
	)

	t.Run("истёкший срок — отмена cleanup'ом", func(t *testing.T) {
		id := f.createIntention(t, ctx, KindCheckIn, "истёкшая тема", -6, bgChatID)
		if _, err := f.pool.Exec(ctx,
			`UPDATE intentions SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatalf("expires_at не сдвинут: %v", err)
		}
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт не удался: %v", err)
		}
		if st := f.intentionStatus(t, ctx, id); st != "cancelled" {
			t.Fatalf("просроченное намерение должно быть cancelled (§14.1 expiresAt), статус %s", st)
		}
	})

	t.Run("идентичное ждущее намерение — дубль отменяется", func(t *testing.T) {
		f, ctx := newFixture(t)
		script := f.withModel(t,
			&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
			&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
		)
		f.seedPrivateChat(t, ctx, "обсудили винил", f.now.Add(-6*time.Hour))
		a := f.createIntention(t, ctx, KindShare, "находка про винил", -6, bgChatID)
		// Второй пояс §14.1: та же (kind, topic, chat) с другим dedupe_key —
		// возможно только при расхождении нормализации между запусками.
		var b int64
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, not_before, expires_at, dedupe_key)
			VALUES ('share', $1, 'НАХОДКА ПРО ВИНИЛ', 'тест второго пояса',
			        (SELECT id FROM provenance LIMIT 1), now() - interval '6 hours', now() + interval '66 hours', 'belt-b')
			RETURNING id`, bgChatID).Scan(&b); err != nil {
			t.Fatalf("второй пояс не вставлен: %v", err)
		}
		// Обе инициативы исполняются сегодня: сдвиг часов мимо 4-часовой паузы
		// §14.3, чтобы проверялся именно дубль-гейт.
		f.setNow(f.now.Add(5 * time.Hour))
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт не удался: %v", err)
		}
		// Первый по claim (меньший id) отменяется как дубль ждущего; выживший
		// исполняется. Оба терминальны.
		if st := f.intentionStatus(t, ctx, a); st != "cancelled" {
			t.Fatalf("дубль должен быть отменён гейтом (§14.1), статус %s", st)
		}
		if st := f.intentionStatus(t, ctx, b); st != "completed" {
			t.Fatalf("выжившее намерение должно исполниться, статус %s", st)
		}
		if got := len(script.Calls()); got != 1 {
			t.Fatalf("модель должна вызваться один раз (выжившим), вызвалась %d", got)
		}
	})

	t.Run("чат ушёл вперёд — намерение неактуально", func(t *testing.T) {
		id := f.createIntention(t, ctx, KindReplyLater, "старая тема", -6, bgChatID)
		// Событие минуту назад: чат активен, инициатива неуместна (§14.3).
		f.seedPrivateChat(t, ctx, "уже другая тема!", f.now.Add(time.Minute))
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт не удался: %v", err)
		}
		if st := f.intentionStatus(t, ctx, id); st != "cancelled" {
			t.Fatalf("инициатива в активном чате должна быть cancelled, статус %s", st)
		}
	})

	// Портировано из TS-линии 132807e (2026-10-31): сравнение с createdAt
	// намерения убивало проактивность от любого сообщения за часы до срока.
	// Теперь инициатива жива, если в чате тихо InitiativeQuietWindow (30 мин).
	t.Run("сообщение старше окна тишины не убивает инициативу", func(t *testing.T) {
		f2, ctx2 := newFixture(t)
		f2.withModel(t,
			&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}})
		// Намерение создано 6 часов назад; сообщение пришло 2 часа назад —
		// после создания, но за пределами 30-минутного окна тишины.
		f2.seedPrivateChat(t, ctx2, "кидай как соберёшь", f2.now.Add(-2*time.Hour))
		id := f2.createIntention(t, ctx2, KindReplyLater, "давно не общались про сборку", -6, bgChatID)
		if err := f2.sched.Tick(ctx2); err != nil {
			t.Fatalf("такт не удался: %v", err)
		}
		if st := f2.intentionStatus(t, ctx2, id); st != "completed" {
			t.Fatalf("инициатива при тишине 2 часа должна исполниться, статус %s", st)
		}
	})

	t.Run("explore не глядит на активность чата", func(t *testing.T) {
		f3, ctx3 := newFixture(t)
		f3.withModel(t,
			&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}})
		// Свежее сообщение минуту назад: для инициативы это отмена, explore —
		// внутреннее действие, чат не трогает.
		f3.seedPrivateChat(t, ctx3, "а вот ещё новость", f3.now.Add(time.Minute))
		id := f3.createIntention(t, ctx3, KindExplore, "тема про термояд", -1, bgChatID)
		if err := f3.sched.Tick(ctx3); err != nil {
			t.Fatalf("такт не удался: %v", err)
		}
		if st := f3.intentionStatus(t, ctx3, id); st == "cancelled" {
			t.Fatal("explore не должен отменяться гейтом активности чата (132807e)")
		}
	})
}

// TestProactiveCaps — проактивная политика §14.3: ≤2 инициативных
// последовательностей на личный чат в сутки, ≥4 часов между, группы выключены.
func TestProactiveCaps(t *testing.T) {
	f, ctx := newFixture(t)
	cfg := f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
	)
	f.seedPrivateChat(t, ctx, "давно не общались про музыку", f.now.Add(-6*time.Hour))

	// 1) первая инициатива проходит и создаёт доставку.
	a := f.createIntention(t, ctx, KindReplyLater, "тема про музыку", -6, bgChatID)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 1 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, a); st != "completed" {
		t.Fatalf("первая инициатива должна исполниться, статус %s", st)
	}
	var outboxCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE chat_id = $1 AND status = 'pending'`, bgChatID).
		Scan(&outboxCount); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outboxCount == 0 {
		t.Fatal("инициатива должна поставить доставку в outbox (§13.1)")
	}

	// 2) вторая раньше 4 часов — отмена (пауза между инициативами).
	b := f.createIntention(t, ctx, KindCheckIn, "как дела", -6, bgChatID)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 2 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, b); st != "cancelled" {
		t.Fatalf("инициатива раньше 4 часов должна быть отменена (§14.3), статус %s", st)
	}

	// 3) через 5 часов — снова можно (сегодня первое исполнение).
	c := f.createIntention(t, ctx, KindShare, "находка про джаз", -6, bgChatID)
	f.setNow(f.now.Add(5 * time.Hour))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 3 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, c); st != "completed" {
		t.Fatalf("после паузы инициатива должна исполниться, статус %s", st)
	}

	// 4) третья за сутки — дневной лимит (§14.3: не более 2).
	d := f.createIntention(t, ctx, KindCheckIn, "просто скучаю", -6, bgChatID)
	f.setNow(f.now.Add(5 * time.Hour))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 4 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, d); st != "cancelled" {
		t.Fatalf("третья инициатива за сутки должна быть отменена, статус %s", st)
	}

	// 5) группы выключены.
	groupChat := int64(-200800)
	f.seedEventStatus(t, ctx, groupChat, "group", "шум группы", f.now.Add(-6*time.Hour), "journal_only")
	e := f.createIntention(t, ctx, KindReplyLater, "тема группы", -6, groupChat)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 5 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, e); st != "cancelled" {
		t.Fatalf("инициатива в группу должна быть отменена (§14.3), статус %s", st)
	}

	if got := len(cfg.Calls()); got != 2 {
		t.Fatalf("модель должна вызваться ровно дважды (два исполнения), вызвалась %d", got)
	}
}

// interruptedExplore — «краш» после generate (§12.2): фазы идут в том же
// порядке, что в runIntention, но до apply дело не доходит. Возвращает
// намерение (running) и id потока задачи.
func interruptedExplore(t *testing.T, f *fixture, ctx context.Context, it Intention) string {
	t.Helper()
	unitID := intentionUnitID(it.ID)
	_, cp, err := f.sched.startTask(ctx, unitID, "тест 9.6", nil)
	if err != nil {
		t.Fatalf("задача не открыта: %v", err)
	}
	cp.Attempt++
	if err := f.sched.saveCheckpoint(ctx, unitID, cp); err != nil {
		t.Fatalf("чекпоинт не записан: %v", err)
	}
	if err := f.wal.PlanStep(ctx, unitID, stepGenerate, "intention_explore", nil); err != nil {
		t.Fatalf("WAL plan не записан: %v", err)
	}
	scope := bgScope // уже "chat:-200700"
	run, err := f.sched.generateExplore(ctx, it, intentionOperation(it.ID, cp.Attempt), scope)
	if err != nil {
		t.Fatalf("генерация не удалась: %v", err)
	}
	raw, err := json.Marshal(run.Result)
	if err != nil {
		t.Fatalf("результат не сериализуется: %v", err)
	}
	if err := f.wal.StepDone(ctx, unitID, stepGenerate,
		map[string]any{"decision": run.Result.Decision, "proposal": json.RawMessage(raw)}); err != nil {
		t.Fatalf("WAL done не записан: %v", err)
	}
	done := walDoneNames(f.walSteps(t, ctx, unitID))
	if !done[stepGenerate] || done[stepApply] {
		t.Fatalf("WAL после сбоя: generate должен быть done, apply нет: %v", done)
	}
	return unitID
}

// interruptedInitiative — «краш» после generate инициативы: ход создан,
// turn_started и model_output записаны, apply не выполнен.
func interruptedInitiative(t *testing.T, f *fixture, ctx context.Context, it Intention) (string, int64) {
	t.Helper()
	unitID := intentionUnitID(it.ID)
	_, cp, err := f.sched.startTask(ctx, unitID, "тест 9.6", nil)
	if err != nil {
		t.Fatalf("задача не открыта: %v", err)
	}
	turnID, err := f.sched.createInitiativeTurn(ctx, it)
	if err != nil {
		t.Fatalf("ход инициативы не создан: %v", err)
	}
	cp.TurnID, cp.Attempt = turnID, cp.Attempt+1
	if err := f.sched.saveCheckpoint(ctx, unitID, cp); err != nil {
		t.Fatalf("чекпоинт не записан: %v", err)
	}
	op := intentionOperation(it.ID, cp.Attempt)
	if err := f.journal.WriteTurnStarted(ctx, &character.TurnStarted{
		TurnID: turnID, ChatID: bgChatID, Kind: "background", BudgetOpID: op,
	}); err != nil {
		t.Fatalf("turn_started не записан: %v", err)
	}
	scope := bgScope // уже "chat:-200700"
	run, err := f.sched.generateInitiative(ctx, it, op, scope)
	if err != nil {
		t.Fatalf("генерация не удалась: %v", err)
	}
	if err := f.journal.WriteModelOutput(ctx, turnID, run.Result,
		map[string]any{"calls": run.Stats.Calls, "tokens": run.Stats.Tokens}, run.Repairs); err != nil {
		t.Fatalf("model_output не записан: %v", err)
	}
	if err := f.wal.StepDone(ctx, unitID, stepGenerate, map[string]any{"decision": run.Result.Decision}); err != nil {
		t.Fatalf("WAL done не записан: %v", err)
	}
	return unitID, turnID
}

// TestTaskResumeFromCheckpoint — возобновление фоновой задачи с чекпоинта/WAL
// (§12.2): сбой между generate и apply; свежий прогон применяет сохранённый
// результат БЕЗ новой генерации (счётчик модельных вызовов не растёт).
func TestTaskResumeFromCheckpoint(t *testing.T) {
	t.Run("exploration: proposal из WAL применяется без генерации", func(t *testing.T) {
		f, ctx := newFixture(t)
		script := f.withModel(t,
			&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}},
		)
		f.seedPrivateChat(t, ctx, "рассказал про винил", f.now.Add(-6*time.Hour))
		id := f.createIntention(t, ctx, KindExplore, "виниловые пластинки", -6, bgChatID)

		claimed := f.claimDirectly(t, ctx)
		if len(claimed) != 1 {
			t.Fatalf("хочу одно claim-нутое намерение: %+v", claimed)
		}
		unitID := interruptedExplore(t, f, ctx, claimed[0])

		// Свежий процесс: recovery §12.2 (running → pending → claim) и
		// возобновление с чекпоинта.
		if n, err := f.intentions.ResetRunning(ctx); err != nil || n != 1 {
			t.Fatalf("recovery running→pending: n=%d err=%v", n, err)
		}
		resumed, err := f.intentions.ClaimDue(ctx, 5)
		if err != nil || len(resumed) != 1 {
			t.Fatalf("повторный claim: %+v err=%v", resumed, err)
		}
		out, err := f.sched.runIntention(ctx, resumed[0])
		if err != nil {
			t.Fatalf("возобновление не удалось: %v", err)
		}
		if out.Outcome != "completed" {
			t.Fatalf("возобновлённая задача должна завершиться: %+v", out)
		}
		if got := len(script.Calls()); got != 1 {
			t.Fatalf("возобновление не должно генерировать заново: модельных вызовов %d, хочу 1", got)
		}

		// Находка exploration легла в память ровно один раз.
		var items int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM memory_items WHERE text LIKE 'Винил — живая тема%'`).Scan(&items); err != nil {
			t.Fatalf("память не читается: %v", err)
		}
		if items != 1 {
			t.Fatalf("находка exploration должна лечь в память один раз, лежит %d", items)
		}
		if st := f.intentionStatus(t, ctx, id); st != "completed" {
			t.Fatalf("намерение должно быть completed, статус %s", st)
		}
		done := walDoneNames(f.walSteps(t, ctx, unitID))
		if !done[stepApply] {
			t.Fatalf("apply не отмечен в WAL: %v", done)
		}
	})

	t.Run("инициатива: model_output из журнала хода применяется без генерации", func(t *testing.T) {
		f, ctx := newFixture(t)
		script := f.withModel(t,
			&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
		)
		f.seedPrivateChat(t, ctx, "давно не общались", f.now.Add(-6*time.Hour))
		id := f.createIntention(t, ctx, KindReplyLater, "вернуться к разговору", -6, bgChatID)

		claimed := f.claimDirectly(t, ctx)
		if len(claimed) != 1 {
			t.Fatalf("хочу одно claim-нутое намерение: %+v", claimed)
		}
		_, turnID := interruptedInitiative(t, f, ctx, claimed[0])

		if n, err := f.intentions.ResetRunning(ctx); err != nil || n != 1 {
			t.Fatalf("recovery running→pending: n=%d err=%v", n, err)
		}
		resumed, err := f.intentions.ClaimDue(ctx, 5)
		if err != nil || len(resumed) != 1 {
			t.Fatalf("повторный claim: %+v err=%v", resumed, err)
		}
		out, err := f.sched.runIntention(ctx, resumed[0])
		if err != nil {
			t.Fatalf("возобновление не удалось: %v", err)
		}
		if out.Outcome != "completed" {
			t.Fatalf("возобновлённая инициатива должна завершиться: %+v", out)
		}
		if got := len(script.Calls()); got != 1 {
			t.Fatalf("возобновление не должно генерировать заново: модельных вызовов %d, хочу 1", got)
		}

		var outbox int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outbox); err != nil {
			t.Fatalf("outbox не читается: %v", err)
		}
		if outbox == 0 {
			t.Fatal("возобновлённая инициатива должна поставить доставку")
		}
		var related *int64
		if err := f.pool.QueryRow(ctx,
			`SELECT related_turn_id FROM intentions WHERE id = $1`, id).Scan(&related); err != nil {
			t.Fatalf("намерение не читается: %v", err)
		}
		if related == nil || *related != turnID {
			t.Fatalf("related_turn_id должен указывать на ход доставки: %v", related)
		}
	})
}

// TestDeferOnDirectWork — перенос фона при прямом обращении (§5.2): из-за
// pending-хода прямого обращения такт не запускает фон вовсе; после применения
// прямого хода намерение исполняется.
func TestDeferOnDirectWork(t *testing.T) {
	f, ctx := newFixture(t)
	script := f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
	)
	f.seedPrivateChat(t, ctx, "обычный разговор", f.now.Add(-6*time.Hour))
	id := f.createIntention(t, ctx, KindReplyLater, "отложенная тема", -6, bgChatID)

	turnID := f.seedDirectTurnPending(t, ctx)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт при прямом обращении не удался: %v", err)
	}
	if got := len(script.Calls()); got != 0 {
		t.Fatalf("при прямом обращении модель вызываться не должна (§5.2), вызовов %d", got)
	}
	if st := f.intentionStatus(t, ctx, id); st != "pending" {
		t.Fatalf("намерение должно остаться в pending (фон отложен), статус %s", st)
	}

	// Прямой ход применён — фон снова может работать. Часы прокручиваются за
	// окно тишины (§14.3): прямой ход только что был, инициатива ждёт тишины.
	if _, err := f.pool.Exec(ctx,
		`UPDATE turns SET status = 'applied', applied_at = now() WHERE id = $1`, turnID); err != nil {
		t.Fatalf("прямой ход не применён: %v", err)
	}
	f.setNow(f.now.Add(InitiativeQuietWindow + time.Minute))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт после прямого обращения не удался: %v", err)
	}
	if got := len(script.Calls()); got != 1 {
		t.Fatalf("после прямого обращения инициатива должна исполниться одним вызовом, вызовов %d", got)
	}
	if st := f.intentionStatus(t, ctx, id); st != "completed" {
		t.Fatalf("инициатива должна исполниться, статус %s", st)
	}
}

// TestInitiativeE2E — полный путь инициативы через такт: ход §12.1, outbox,
// CAS намерения, бюджет пула background (§16), память из результата.
func TestInitiativeE2E(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}},
	)
	f.seedPrivateChat(t, ctx, "обсудили музыку", f.now.Add(-6*time.Hour))
	id := f.createIntention(t, ctx, KindShare, "поделиться находкой", -6, bgChatID)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, id); st != "completed" {
		t.Fatalf("инициатива должна завершиться, статус %s", st)
	}

	// Ход инициативы: применён, доставка ждёт отправки.
	var turnID int64
	if err := f.pool.QueryRow(ctx,
		`SELECT related_turn_id FROM intentions WHERE id = $1`, id).Scan(&turnID); err != nil || turnID == 0 {
		t.Fatalf("связанный ход инициативы не найден: %v", err)
	}
	var msgs int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND status = 'pending'`, turnID).
		Scan(&msgs); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if msgs != 1 {
		t.Fatalf("одна доставка инициативы, в outbox %d", msgs)
	}

	// Журнал хода §12.1: полный набор событий фонового хода.
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал хода не читается: %v", err)
	}
	var types []string
	for _, ev := range evs {
		types = append(types, ev.EventType())
	}
	for _, want := range []string{"turn_started", "model_output", "deltas_applied", "delivery_queued"} {
		found := false
		for _, tp := range types {
			if tp == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("в журнале хода инициативы нет %s: %v", want, types)
		}
	}

	// Бюджет §16: резерв в пуле background, не в conversation.
	var poolName string
	if err := f.pool.QueryRow(ctx,
		`SELECT pool FROM budget_reservations WHERE operation_id LIKE 'model:bg:intention:%' LIMIT 1`).
		Scan(&poolName); err != nil {
		t.Fatalf("резерв бюджета не найден: %v", err)
	}
	if poolName != "background" {
		t.Fatalf("инициатива должна резервировать пул background, резерв в %q (§16)", poolName)
	}

	// Память из результата применена.
	var mem int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE text LIKE 'Коля ищет редкий%'`).Scan(&mem); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if mem != 1 {
		t.Fatalf("кандидат памяти инициативы должен примениться, записей %d", mem)
	}
}

// TestConsolidationE2E — консолидация §10.2: готовый эпизод сводится в summary,
// события помечаются, stances применяются с основаниями runtime; повторная
// консолидация не дублирует и не самоподтверждается (§10.3).
func TestConsolidationE2E(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Эпизод про винил: Коля показал коллекцию и рассказал о редком прессе.",
			"Собирает винил, гордится коллекцией")}},
	)
	ids, itemID := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт консолидации не удался: %v", err)
	}

	// События помечены summarized_at — эпизод закрыт.
	var unsummarized int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).
		Scan(&unsummarized); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if unsummarized != 0 {
		t.Fatalf("все события эпизода должны быть помечены, не помечено %d", unsummarized)
	}

	// Summary — experience без confidence (§10.2: не превращает предположение
	// в факт; §10.3: повторное чтение не повышает уверенность).
	var summaryID int64
	var confidence *float64
	var salience float64
	if err := f.pool.QueryRow(ctx, `
		SELECT id, confidence, salience FROM memory_items
		WHERE kind = 'experience' AND text LIKE 'Эпизод про винил%'`).
		Scan(&summaryID, &confidence, &salience); err != nil {
		t.Fatalf("summary не найдено: %v", err)
	}
	_ = summaryID
	if confidence != nil {
		t.Fatal("summary не должно иметь confidence (§10.2)")
	}
	if salience != 0.3 {
		t.Fatalf("salience summary = %v, хочу 0.3", salience)
	}

	// Stance применён runtime с основаниями из памяти эпизода (§10.2).
	var strength, interest float64
	var revision int
	var stanceID int64
	if err := f.pool.QueryRow(ctx, `
		SELECT id, strength, interest, revision FROM stances
		WHERE scope_id = $1 AND aspect = 'general'`, bgScope).
		Scan(&stanceID, &strength, &interest, &revision); err != nil {
		t.Fatalf("stance не найден: %v", err)
	}
	if strength != 0.6 || interest != 0.7 || revision != 0 {
		t.Fatalf("stance не тот: strength=%v interest=%v revision=%d (первая вставка даёт revision 0, инкремент — на пересмотре)", strength, interest, revision)
	}
	var basis int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM stance_basis WHERE stance_id = $1 AND memory_id = $2`, stanceID, itemID).
		Scan(&basis); err != nil {
		t.Fatalf("основания не читаются: %v", err)
	}
	if basis != 1 {
		t.Fatalf("основание stance должно ссылаться на память эпизода, найдено %d", basis)
	}

	// Повторное применение того же эпизода — no-op: дублей нет.
	proposal, err := parseConsolidationProposal([]byte(consolidationJSON(
		"Эпизод про винил: Коля показал коллекцию и рассказал о редком прессе.",
		"Собирает винил, гордится коллекцией")))
	if err != nil {
		t.Fatalf("повторный proposal не валиден: %v", err)
	}
	ep := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids, LastEventID: ids[len(ids)-1]}
	applied, _, err := f.sched.applyConsolidation(ctx, ep, proposal)
	if err != nil {
		t.Fatalf("повторное применение не должно падать: %v", err)
	}
	if applied {
		t.Fatal("повторное применение эпизода должно быть no-op (события уже summarized)")
	}
	var summaries int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE kind = 'experience' AND text LIKE 'Эпизод про винил%'`).
		Scan(&summaries); err != nil {
		t.Fatalf("summary не читается: %v", err)
	}
	if summaries != 1 {
		t.Fatalf("summary должно быть ровно одно, найдено %d", summaries)
	}

	// §10.3: новое чтение тех же источников с тем же резюме stance — не
	// ревизия: revision и strength не растут. Новый эпизод, то же резюме.
	ids2, _ := f.seedEpisode(t, ctx, bgChatID, "private", 2, f.now.Add(-10*time.Hour))
	ep2 := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids2, LastEventID: ids2[len(ids2)-1]}
	applied2, _, err := f.sched.applyConsolidation(ctx, ep2, proposal)
	if err != nil || !applied2 {
		t.Fatalf("новый эпизод должен консолидироваться: applied=%v err=%v", applied2, err)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT strength, revision FROM stances WHERE id = $1`, stanceID).
		Scan(&strength, &revision); err != nil {
		t.Fatalf("stance не перечитан: %v", err)
	}
	if revision != 0 || strength != 0.6 {
		t.Fatalf("повторное чтение тех же источников не должно пересматривать stance (§10.3): revision=%d strength=%v", revision, strength)
	}

	// WAL след задачи консолидации: collect → generate → apply (§12.2).
	done := walDoneNames(f.walSteps(t, ctx, consolidationUnitID(bgChatID, ids[len(ids)-1])))
	if !done[stepCollect] || !done[stepGenerate] || !done[stepApply] {
		t.Fatalf("WAL консолидации неполный: %v", done)
	}
}

// TestExplorationDailyCap — дневной лимит exploration (§16: до 2 в сутки).
func TestExplorationDailyCap(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}},
	)
	f.seedPrivateChat(t, ctx, "тема про технику", f.now.Add(-6*time.Hour))

	a := f.createIntention(t, ctx, KindExplore, "техника: винил", -6, bgChatID)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 1 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, a); st != "completed" {
		t.Fatalf("первое exploration должно исполниться, статус %s", st)
	}
	b := f.createIntention(t, ctx, KindExplore, "техника: проигрыватели", -6, bgChatID)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 2 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, b); st != "completed" {
		t.Fatalf("второе exploration должно исполниться, статус %s", st)
	}
	c := f.createIntention(t, ctx, KindExplore, "техника: иглы", -6, bgChatID)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт 3 не удался: %v", err)
	}
	if st := f.intentionStatus(t, ctx, c); st != "cancelled" {
		t.Fatalf("третье exploration за сутки должно быть отменено (§16), статус %s", st)
	}

	// exploration не создаёт доставок: находки остаются в памяти (§15).
	var deliveries int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE chat_id = $1`, bgChatID).Scan(&deliveries); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if deliveries != 0 {
		t.Fatalf("exploration не должен отправлять сообщения (§15), в outbox %d", deliveries)
	}
}

// TestQueueStatsAndObservability — наблюдаемость §20: снимок очереди,
// предупреждение на рост очереди, heartbeat на debug.
func TestQueueStatsAndObservability(t *testing.T) {
	f, ctx := newFixture(t)

	var buf bytes.Buffer
	s, err := New(Deps{
		Pool:        f.pool,
		Journal:     f.journal,
		WAL:         f.wal,
		Checkpoints: f.checkpoints,
		Budget:      f.budget,
		Logger:      log.New(&buf, "", 0),
		Now:         func() time.Time { return f.now },
	}, Options{})
	if err != nil {
		t.Fatalf("планировщик не создан: %v", err)
	}

	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("queueStats не читаются: %v", err)
	}
	if stats.PendingTurns != 0 || stats.PendingOutbox != 0 || stats.DueIntentions != 0 {
		t.Fatalf("пустая очередь: %+v", stats)
	}

	// Ход в очереди и намерение due видны.
	_ = f.seedDirectTurnPending(t, ctx)
	_ = f.createIntention(t, ctx, KindReplyLater, "due-тема", -6, bgChatID)
	stats, err = s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("queueStats не читаются: %v", err)
	}
	if stats.PendingTurns != 1 || stats.DueIntentions != 1 {
		t.Fatalf("очередь с прямым ходом и due-намерением: %+v", stats)
	}

	// Рост очереди — предупреждение после growthWarnTicks тактов; heartbeat —
	// каждый такт.
	for i := 0; i < growthWarnTicks+1; i++ {
		s.observeTick(QueueStats{PendingTurns: 1 + i})
	}
	logged := buf.String()
	if !strings.Contains(logged, "рост очереди") {
		t.Fatalf("рост очереди должен предупреждать оператора (§20), лог: %s", logged)
	}
	if !strings.Contains(logged, "heartbeat") {
		t.Fatalf("heartbeat должен писаться каждым тактом, лог: %s", logged)
	}

	// Сокращение очереди снимает предупреждение: новый эпизод роста предупреждает
	// снова только после полной серии тактов.
	buf.Reset()
	for i := 0; i < growthWarnTicks+1; i++ {
		s.observeTick(QueueStats{PendingTurns: 1 + i})
	}
	if !strings.Contains(buf.String(), "рост очереди") {
		t.Fatalf("после сокращения и нового роста предупреждение должно вернуться, лог: %s", buf.String())
	}
}

// --- хелперы ------------------------------------------------------------------

func mustTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	return tx
}

// claimDirectly — атомарный claim всех due-намерений (обход Tick для точечных тестов).
func (f *fixture) claimDirectly(t *testing.T, ctx context.Context) []Intention {
	t.Helper()
	got, err := f.intentions.ClaimDue(ctx, 5)
	if err != nil {
		t.Fatalf("claim не удался: %v", err)
	}
	return got
}
