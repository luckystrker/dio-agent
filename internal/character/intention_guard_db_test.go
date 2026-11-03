//go:build db

// БД-тест страховок намерений (26.2): runtime поднимает задержку до пола по
// виду и отклоняет тему-мусор, не роняя ход.
package character

import (
	"testing"
	"time"
)

func TestApplyIntentionGuardsDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(7001)
	f.seedTurn(t, ctx, turnID, "как ты там?")

	started := time.Now()
	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:  turnID,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result: TurnResult{
			Decision: DecisionIgnore,
			Intentions: []IntentionProposal{
				{Kind: "check_in", Topic: "как ты", Reason: "проверить", DelayHours: 0.094},
				{Kind: "share", Topic: "находка про винил", Reason: "поделиться", DelayHours: 0.069},
				{Kind: "reply_later", Topic: "пластинки", Reason: "вернуться", DelayHours: 0.078},
				{Kind: "explore", Topic: "эпилепсия", Reason: "изучить", DelayHours: 0.05},
				{Kind: "check_in", Topic: "placeholder1", Reason: "мусор", DelayHours: 5},
				{Kind: "share", Topic: "🙂", Reason: "мусор", DelayHours: 5},
			},
		},
		Journal: f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}
	if len(res.IntentionIDs) != 4 {
		t.Fatalf("создано намерений %d, хочу 4 (два мусорных отклонены)", len(res.IntentionIDs))
	}
	if len(res.RejectedIntentions) != 2 {
		t.Fatalf("отклонено мусорных тем %d, хочу 2: %+v", len(res.RejectedIntentions), res.RejectedIntentions)
	}
	if len(res.RaisedIntentions) != 3 {
		t.Fatalf("поднято до пола %d, хочу 3: %+v", len(res.RaisedIntentions), res.RaisedIntentions)
	}

	wantFloor := map[string]time.Duration{
		"check_in":    2 * time.Hour,
		"share":       15 * time.Minute,
		"reply_later": 6 * time.Minute,
		"explore":     3 * time.Minute,
	}
	for kind, floor := range wantFloor {
		var notBefore time.Time
		if err := f.pool.QueryRow(ctx,
			`SELECT not_before FROM intentions WHERE kind = $1 AND target_chat_id = $2`,
			kind, testChatID).Scan(&notBefore); err != nil {
			t.Fatalf("намерение %s не найдено: %v", kind, err)
		}
		got := notBefore.Sub(started)
		if got < floor-45*time.Second || got > floor+time.Minute {
			t.Errorf("%s: задержка %v, хочу около %v (пол вида, 26.2)", kind, got, floor)
		}
	}

	// Мусорные темы не создали строк даже в статусе pending.
	var garbage int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE topic IN ('placeholder1', '🙂')`).Scan(&garbage); err != nil {
		t.Fatalf("мусорные намерения не посчитаны: %v", err)
	}
	if garbage != 0 {
		t.Fatalf("мусорные намерения всё же созданы: %d", garbage)
	}
}
