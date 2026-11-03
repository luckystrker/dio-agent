//go:build db

// Поведенческие проверки механик по docs/BEHAVIOR_SCENARIOS.md — на живых
// модулях приложения со скриптованными диалогами. Эталоны тона принимаются
// вручную (Р-критерий); здесь — механическая сторона каждого эталона:
//   - молчание вместо автоматического ответа (сценарий 1): ignore без доставки;
//   - реакция без текста (сценарий 3): setMessageReaction вместо sendMessage;
//   - вежливость с незнакомцем (сценарий 5): обычная обработка + карточка
//     отношений с нуля (anti-assistant ≠ anti-helpfulness);
//   - ответ на прямой вопрос при подавленной инициативе (чек-лист 4.6, §21.2):
//     инициатива по теме гасится «чат ушёл вперёд», прямой вопрос отвечает.
package app

import (
	"context"
	"strings"
	"testing"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// TestBehaviorSilenceInsteadOfAutoReply — сценарий 1 эталонов: реплика без
// вопроса — персонаж может не ответить; следующая реплика получает ответ.
// Провал-механика: автоматический ответ на каждую реплику подряд.
func TestBehaviorSilenceInsteadOfAutoReply(t *testing.T) {
	chat := int64(200801)
	h := newHarness(t,
		textResp(`{"decision":"ignore","reason":"реплика без вопроса — говорить нечего"}`),
		textResp(`{"decision":"answer","messages":["ты долбоёб", "а что переписываешь"],"reason":"свой, тон допускает"}`),
	)

	// Первая реплика: тишина.
	h.api.push(1, chat, "private", "я короче решил всё переписать")
	eventually(t, "первый ход обработан (события закрыты)", func() bool {
		return queryInt(h, `SELECT count(*) FROM social_events WHERE chat_id = $1 AND status = 'processed'`, chat) > 0
	})
	turn1 := queryInt(h, `SELECT id FROM turns WHERE chat_id = $1 ORDER BY id LIMIT 1`, chat)
	if status := queryString(h, `SELECT status FROM turns WHERE id = $1`, turn1); status != "applied" {
		t.Fatalf("первый ход не применён: %s", status)
	}
	if n := queryInt(h, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turn1); n != 0 {
		t.Fatalf("ignore породил доставку (%d строк) — автоматический ответ, провал", n)
	}

	// Вторая реплика: грубоватое участие допустимо persona и отношениями.
	h.api.push(2, chat, "private", "прям вообще всё")
	eventually(t, "ответ на вторую реплику доставлен", func() bool {
		return len(h.api.callsOf("sendMessage")) == 2
	})
	sent := h.api.callsOf("sendMessage")
	if sent[0].Text != "ты долбоёб" || sent[1].Text != "а что переписываешь" {
		t.Fatalf("порядок/содержание bubbles не те: %+v", sent)
	}
}

// TestBehaviorReactionWithoutText — сценарий 3: сочувствие эмодзи вместо текста;
// реакция ставится на сообщение собеседника, sendMessage не вызывается.
func TestBehaviorReactionWithoutText(t *testing.T) {
	chat := int64(200802)
	h := newHarness(t,
		textResp(`{"decision":"react","reaction":"😭","reason":"живая реакция вместо абзаца сочувствия"}`),
	)

	h.api.push(1, chat, "private", "я уже третий час ковыряю баг")
	turnID := waitForTurn(t, h, chat)
	eventually(t, "реакция доставлена", func() bool {
		return len(h.api.callsOf("setMessageReaction")) == 1
	})

	// Цель реакции — сообщение собеседника (target_message_id из события).
	call := h.api.callsOf("setMessageReaction")[0]
	target := queryInt(h, `SELECT (raw_update->'message'->>'message_id')::bigint FROM social_events WHERE turn_id = $1`, turnID)
	if call.ChatID != chat || call.MessageID != target || call.Emoji != "😭" {
		t.Fatalf("реакция не на то сообщение: %+v (хочу chat=%d message=%d)", call, chat, target)
	}
	if n := len(h.api.callsOf("sendMessage")); n != 0 {
		t.Fatalf("react написал текст (%d sendMessage) — провал эталона", n)
	}
	if n := queryInt(h, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'reaction'`, turnID); n != 1 {
		t.Fatalf("в outbox нет единицы реакции: %d", n)
	}
}

// TestBehaviorPolitenessToStranger — сценарий 5: незнакомец получает живой
// ответ (не грубость и не демонстративный отказ); механика: карточка человека
// и отношения создаются с нуля, обработка ничем не отличается от своего.
func TestBehaviorPolitenessToStranger(t *testing.T) {
	chat := int64(200803)
	h := newHarness(t,
		textResp(`{"decision":"answer","messages":["а что там"],"reason":"помочь не стыдно"}`),
	)

	h.api.pushAs(1, chat, "private", 555, "Незнакомец", "привет, можешь помочь с кодом?")
	eventually(t, "ответ незнакомцу доставлен", func() bool {
		return len(h.api.callsOf("sendMessage")) == 1
	})
	if got := h.api.callsOf("sendMessage")[0].Text; got != "а что там" {
		t.Fatalf("ответ не тот: %q", got)
	}

	// Карточка незнакомца и отношения — с нуля (§7.3): familiarity выросла на
	// шаг значимого хода, affinity/trust при стартовых значениях. Ключ актора
	// M7 — user:<id>, «Незнакомец» остаётся отображаемым именем-алиасом.
	entityID := queryInt(h, `
		SELECT id FROM entities WHERE canonical_name = 'user:555' AND scope_id = 'chat:200803'`)
	if entityID == 0 {
		t.Fatalf("сущность незнакомца не создана")
	}
	fam := queryInt(h, `
		SELECT (familiarity * 100)::bigint FROM relationships
		WHERE person_entity_id = $1 AND scope_id = 'chat:200803'`, entityID)
	if fam == 0 {
		t.Fatalf("familiarity незнакомца не выросла — ход не значим для отношений")
	}
	affinity := queryString(h, `SELECT affinity::text FROM relationships WHERE person_entity_id = $1`, entityID)
	if affinity != "0" {
		t.Fatalf("affinity незнакомца не стартовый: %s", affinity)
	}
}

// TestBehaviorDirectQuestionBeatsSuppressedInitiative — чек-лист 4.6 + §21.2:
// отложенная инициатива по теме не пишёт «починил?» поверх свежего разговора
// (гейт «чат ушёл вперёд» §14.1), а прямой вопрос получает ответ с памятью.
func TestBehaviorDirectQuestionBeatsSuppressedInitiative(t *testing.T) {
	chat := int64(200804)
	h := newHarness(t,
		toolResp("c-q1", "memory_search", `{"query":"проект переписывание"}`),
		textResp(`{"decision":"answer","messages":["помню: ты переписываешь проект с нуля"],"reason":"прямой вопрос — отвечаю"}`),
	)

	// Прямой вопрос: ответ обязателен, инициативы не помеха (§10.4: принудительный
	// первый вызов памяти; здесь — скрипт с инструментным раундом).
	h.api.push(1, chat, "private", "ну как там моя штука, помнишь?")
	eventually(t, "прямой вопрос отвечен", func() bool {
		return len(h.api.callsOf("sendMessage")) == 1
	})
	if got := h.api.callsOf("sendMessage")[0].Text; !strings.Contains(got, "помню") {
		t.Fatalf("прямой вопрос без ответа: %q", got)
	}

	// Отложенная инициатива по той же теме, срок уже пришёл. Чат активен
	// (вопрос и ответ — минуты назад): гейт тишины гасит её без сообщения
	// пользователю (§14.3; семантика TS-линии 132807e).
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(context.Background(), h.pool,
		memory.SourceJournalEvent, "turn:0", []string{memory.ChatScope("200804")})
	if err != nil {
		t.Fatalf("provenance намерения не создан: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, status, not_before, expires_at, dedupe_key, created_at, updated_at)
		VALUES ('check_in', $1, 'проект', 'вернуться к переписыванию', $2, 'pending',
		        now() - interval '1 second', now() + interval '24 hours',
		        '["check_in","проект",200804]', now() - interval '1 hour', now() - interval '1 hour')`,
		chat, provenanceID); err != nil {
		t.Fatalf("инициатива не посеяна: %v", err)
	}

	// Инициатива погашена гейтом — второго сообщения о теме нет (§21.2: повторы
	// темы подавляются независимо от доступности памяти).
	eventually(t, "инициатива погашена гейтом тишины", func() bool {
		st, ok := queryStringOK(h, `SELECT status FROM intentions WHERE kind = 'check_in' AND target_chat_id = $1`, chat)
		return ok && (st == "cancelled" || st == "failed")
	})
	if n := len(h.api.callsOf("sendMessage")); n != 1 {
		t.Fatalf("инициатива написала поверх свежего ответа: %d sendMessage, хочу 1", n)
	}
	if reason := queryString(h, `SELECT coalesce(reason,'') FROM intentions WHERE kind = 'check_in' AND target_chat_id = $1`, chat); !strings.Contains(reason, "чат активен") {
		t.Fatalf("инициатива погашена не по «чат ушёл вперёд»: %q", reason)
	}
}

// compile-time: контракт результата используется сценариями выше.
