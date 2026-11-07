//go:build db

// M1 ревью 2026-11-07: все потребители Bot API — включая typing-пульс —
// получают уже обёрнутый GuardAPI. Иначе «печатает…» по остаточному ходу
// уходит в чужую личку мимо запрета этапа 42.
package app

import (
	"context"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

func TestTypingPulseGoesThroughGuardAPI(t *testing.T) {
	const friend, stranger = int64(722958928), int64(42)
	h := newHarnessTuned(t, func(o *Options) {
		o.Config.Telegram = &runtime.TelegramSettings{PrivateAllowlist: []int64{friend}}
	})
	if h.app.runner.Typing == nil {
		t.Fatal("telegram настроен — typing-пульс должен быть собран")
	}

	// Разрешённый чат доказывает, что пульс жив и шлёт chat action.
	h.app.runner.Typing.Start(friend)
	eventually(t, "typing разрешённому чату", func() bool {
		return len(h.api.callsOf("sendChatAction")) > 0
	})

	// Чужой чат: вызов до пульса доходит, до сырого API — нет.
	h.app.runner.Typing.Start(stranger)
	time.Sleep(50 * time.Millisecond)
	h.app.runner.Typing.Stop(friend)
	h.app.runner.Typing.Stop(stranger)

	for _, call := range h.api.callsOf("sendChatAction") {
		if call.ChatID == stranger {
			t.Fatalf("typing ушёл в чужую личку %d мимо GuardAPI", stranger)
		}
	}
}

// M2 ревью 2026-11-07: App.Start сам снимает пометку whitelist'а с id,
// вернувшихся в список, и не трогает настоящую блокировку Telegram. Юнит-тест
// функции этого не доказывает — проверяем проводку старта.
func TestStartupUnblocksRelistedChatButKeepsRealBlocks(t *testing.T) {
	const friend, realBlocked = int64(722958928), int64(555)
	pool, dsn := newTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO blocked_chats (chat_id, reason) VALUES
		  ($1, 'Forbidden: private chat is not in allowlist'),
		  ($2, 'Forbidden: bot was blocked by the user')`,
		friend, realBlocked); err != nil {
		t.Fatalf("пометки не посеяны: %v", err)
	}

	newHarnessDSNTuned(t, dsn, func(o *Options) {
		// Настоящая блокировка — у id, который снова в списке: проверяем
		// предикат reason, а не отсечение по chat_id.
		o.Config.Telegram = &runtime.TelegramSettings{PrivateAllowlist: []int64{friend, realBlocked}}
	})

	var relelisted, genuine int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocked_chats WHERE chat_id = $1`, friend).Scan(&relelisted); err != nil {
		t.Fatalf("пометка вернувшегося id не прочитана: %v", err)
	}
	if relelisted != 0 {
		t.Fatalf("App.Start не снял пометку whitelist'а с вернувшегося в список id")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocked_chats WHERE chat_id = $1`, realBlocked).Scan(&genuine); err != nil {
		t.Fatalf("настоящая блокировка не прочитана: %v", err)
	}
	if genuine != 1 {
		t.Fatalf("App.Start снял настоящую блокировку Telegram (403 пользователя)")
	}
}

// M1 ревью 2026-11-07: delivery — третий потребитель Bot API — тоже обязан
// получить GuardAPI. Пузырь в чужую личку не доходит до сырого клиента,
// строка становится failed (403 без повторов), чат помечается блокировкой.
func TestDeliveryToForeignPrivateGoesThroughGuardAPI(t *testing.T) {
	const friend, stranger = int64(722958928), int64(42)
	h := newHarnessTuned(t, func(o *Options) {
		o.Config.Telegram = &runtime.TelegramSettings{PrivateAllowlist: []int64{friend}}
	})
	ctx := context.Background()

	if _, err := h.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, attempts)
		VALUES (1, 0, $1, '{"kind":"message","text":"в чужую личку"}'::jsonb, 'pending', 0)`,
		stranger); err != nil {
		t.Fatalf("пузырь доставки не посеян: %v", err)
	}

	eventually(t, "доставка в чужую личку отклонена", func() bool {
		st, ok := queryStringOK(h, `SELECT status FROM delivery_outbox WHERE chat_id = $1`, stranger)
		return ok && st == "failed"
	})
	for _, call := range h.api.callsOf("sendMessage") {
		if call.ChatID == stranger {
			t.Fatalf("delivery отправил в чужую личку %d мимо GuardAPI", stranger)
		}
	}
	if n := queryInt(h, `SELECT count(*) FROM blocked_chats WHERE chat_id = $1`, stranger); n != 1 {
		t.Fatalf("после отказа доставки чат не помечен блокировкой: записей %d", n)
	}
}
