package background

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"
)

// TestReadyEpisodesBoundaries — границы эпизода §10.2: 4 часа тишины в DM,
// 1 час в группе, 100 событий; внутренняя пауза режет эпизод даже при живом
// чате. Это граница summary, а не задержка памяти.
func TestReadyEpisodesBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	mk := func(id int64, chatID int64, chatType string, at time.Time) episodeEvent {
		return episodeEvent{ID: id, ChatID: chatID, ChatType: chatType, OccurredAt: at}
	}

	t.Run("личный чат: 4 часа тишины закрывают эпизод", func(t *testing.T) {
		evs := []episodeEvent{
			mk(1, 100, "private", now.Add(-5*time.Hour)),
			mk(2, 100, "private", now.Add(-4*time.Hour-time.Minute)),
		}
		got := readyEpisodes(now, evs)
		if len(got) != 1 || len(got[0].EventIDs) != 2 {
			t.Fatalf("хочу 1 эпизод из 2 событий, получено %+v", got)
		}
		if got[0].LastEventID != 2 {
			t.Fatalf("LastEventID=%d, хочу 2", got[0].LastEventID)
		}
	})

	t.Run("личный чат: тишины меньше 4 часов — эпизод не готов", func(t *testing.T) {
		evs := []episodeEvent{
			mk(1, 100, "private", now.Add(-3*time.Hour)),
		}
		if got := readyEpisodes(now, evs); len(got) != 0 {
			t.Fatalf("свежий эпизод не должен быть готов: %+v", got)
		}
	})

	t.Run("группа: эпизод закрывается часом тишины", func(t *testing.T) {
		evs := []episodeEvent{
			mk(1, 200, "group", now.Add(-2*time.Hour)),
			mk(2, 200, "group", now.Add(-90*time.Minute)),
		}
		got := readyEpisodes(now, evs)
		if len(got) != 1 || len(got[0].EventIDs) != 2 {
			t.Fatalf("группа: хочу 1 готовый эпизод, получено %+v", got)
		}
	})

	t.Run("группа: меньше часа тишины — не готов", func(t *testing.T) {
		evs := []episodeEvent{mk(1, 200, "group", now.Add(-30*time.Minute))}
		if got := readyEpisodes(now, evs); len(got) != 0 {
			t.Fatalf("свежая группа не должна быть готова: %+v", got)
		}
	})

	t.Run("внутренняя пауза режет эпизод при живом чате", func(t *testing.T) {
		// Кусок [1,2] с внутренним зазором в час замолчал на 7 часов, потом чат
		// ожил: готовый старый кусок консолидируется, свежий хвост — нет;
		// целостность чата тишину старого куска не отменяет (§10.2).
		evs := []episodeEvent{
			mk(1, 100, "private", now.Add(-10*time.Hour)),
			mk(2, 100, "private", now.Add(-9*time.Hour)),
			mk(3, 100, "private", now.Add(-2*time.Hour)),
		}
		got := readyEpisodes(now, evs)
		if len(got) != 1 {
			t.Fatalf("хочу только старый эпизод, получено %d: %+v", len(got), got)
		}
		if len(got[0].EventIDs) != 2 || got[0].EventIDs[0] != 1 || got[0].EventIDs[1] != 2 {
			t.Fatalf("старый эпизод не те события: %+v", got[0].EventIDs)
		}
	})

	t.Run("100 событий — граница и без тишины", func(t *testing.T) {
		evs := make([]episodeEvent, 0, 130)
		for i := 0; i < 130; i++ {
			evs = append(evs, mk(int64(i+1), 300, "private", now.Add(-time.Duration(130-i)*time.Minute)))
		}
		got := readyEpisodes(now, evs)
		// 130 событий: кусок [1..100] готов по границе 100, [101..130] ждёт тишину.
		if len(got) != 1 {
			t.Fatalf("хочу 1 готовый кусок, получено %d: %+v", len(got), got)
		}
		if len(got[0].EventIDs) != EpisodeMaxEvents {
			t.Fatalf("кусок не %d событий: %d", EpisodeMaxEvents, len(got[0].EventIDs))
		}
		if got[0].EventIDs[0] != 1 || got[0].EventIDs[99] != 100 {
			t.Fatalf("кусок не первые 100 событий: %v..%v", got[0].EventIDs[0], got[0].EventIDs[99])
		}
	})

	t.Run("250 событий без тишины дают два готовых куска", func(t *testing.T) {
		evs := make([]episodeEvent, 0, 250)
		for i := 0; i < 250; i++ {
			evs = append(evs, mk(int64(i+1), 400, "private", now.Add(-time.Duration(250-i)*time.Minute)))
		}
		got := readyEpisodes(now, evs)
		if len(got) != 2 {
			t.Fatalf("хочу 2 готовых куска по 100, получено %d", len(got))
		}
	})

	t.Run("чаты не смешиваются", func(t *testing.T) {
		evs := []episodeEvent{
			mk(1, 100, "private", now.Add(-5*time.Hour)),
			mk(2, 500, "group", now.Add(-2*time.Hour)),
		}
		got := readyEpisodes(now, evs)
		if len(got) != 2 {
			t.Fatalf("хочу 2 эпизода разных чатов, получено %d", len(got))
		}
	})
}

// TestReadyEpisodesExactChunks — ровно N×100 событий без тишины: нарезка
// закрывает серию целиком, пустого хвоста не остаётся. Раньше на ровно
// 100/200/300 seg после нарезки был пуст и seg[len(seg)-1] паниковал
// (index out of range) — паника в горутине планировщика роняла процесс.
func TestReadyEpisodesExactChunks(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// Непрерывная серия из n событий (пауза 1 мин « границы), последнее — сейчас:
	// тишины нет, готовность даёт только граница 100 событий.
	series := func(chatID int64, n int) []episodeEvent {
		evs := make([]episodeEvent, 0, n)
		for i := 0; i < n; i++ {
			evs = append(evs, episodeEvent{
				ID: int64(i + 1), ChatID: chatID, ChatType: "private",
				OccurredAt: now.Add(-time.Duration(n-i) * time.Minute),
			})
		}
		return evs
	}

	t.Run("ровно 100 — один готовый кусок, без паники", func(t *testing.T) {
		got := readyEpisodes(now, series(600, 100))
		if len(got) != 1 || len(got[0].EventIDs) != 100 {
			t.Fatalf("хочу 1 кусок из 100, получено %d кусков: %+v", len(got), got)
		}
		if got[0].LastEventID != 100 {
			t.Fatalf("LastEventID=%d, хочу 100", got[0].LastEventID)
		}
	})

	t.Run("ровно 200 — два готовых куска", func(t *testing.T) {
		got := readyEpisodes(now, series(700, 200))
		if len(got) != 2 {
			t.Fatalf("хочу 2 куска по 100, получено %d", len(got))
		}
		if got[0].EventIDs[99] != 100 || got[1].EventIDs[0] != 101 || got[1].LastEventID != 200 {
			t.Fatalf("куски не подряд [1..100][101..200]: %+v", got)
		}
	})

	t.Run("ровно 300 — три готовых куска", func(t *testing.T) {
		got := readyEpisodes(now, series(800, 300))
		if len(got) != 3 {
			t.Fatalf("хочу 3 куска по 100, получено %d", len(got))
		}
		if got[2].LastEventID != 300 {
			t.Fatalf("LastEventID последнего куска=%d, хочу 300", got[2].LastEventID)
		}
	})

	t.Run("300+хвост 37 — три куска, хвост ждёт тишину", func(t *testing.T) {
		got := readyEpisodes(now, series(900, 337))
		if len(got) != 3 {
			t.Fatalf("хочу 3 готовых куска, получено %d", len(got))
		}
		for i, ep := range got {
			if len(ep.EventIDs) != 100 {
				t.Fatalf("кусок %d не из 100 событий: %d", i, len(ep.EventIDs))
			}
		}
	})

	t.Run("соседние сегменты 99/100/101", func(t *testing.T) {
		// Три сегмента одного чата с паузами больше 4 ч между ними: 99 закрывается
		// тишиной, ровно 100 — границей, 101 даёт кусок 100 и хвост-одно-событие,
		// который ждёт тишину.
		var evs []episodeEvent
		id := int64(0)
		addSeg := func(n int, start time.Time) {
			for i := 0; i < n; i++ {
				id++
				evs = append(evs, episodeEvent{
					ID: id, ChatID: 950, ChatType: "private",
					OccurredAt: start.Add(time.Duration(i) * time.Second),
				})
			}
		}
		addSeg(99, now.Add(-10*time.Hour))
		addSeg(100, now.Add(-5*time.Hour))
		addSeg(101, now.Add(-30*time.Minute))
		got := readyEpisodes(now, evs)
		if len(got) != 3 {
			t.Fatalf("хочу 3 готовых куска (99 тишиной + 100 + 100), получено %d: %+v", len(got), got)
		}
		for i, want := range []int{99, 100, 100} {
			if n := len(got[i].EventIDs); n != want {
				t.Fatalf("кусок %d: %d событий, хочу %d", i, n, want)
			}
		}
	})
}

// TestProactiveAllowedPolicy — проактивная политика §14.3: только личные чаты,
// не более 2 на чат в сутки, не менее 4 часов между инициативами.
func TestProactiveAllowedPolicy(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	t.Run("группы выключены", func(t *testing.T) {
		if ok, _ := ProactiveAllowed(false, proactiveUsage{}, now); ok {
			t.Fatal("инициатива в группу должна быть запрещена (§14.3)")
		}
	})
	t.Run("пустая история разрешает", func(t *testing.T) {
		if ok, _ := ProactiveAllowed(true, proactiveUsage{}, now); !ok {
			t.Fatal("чистая история должна разрешать инициативу")
		}
	})
	t.Run("лимит суток", func(t *testing.T) {
		if ok, reason := ProactiveAllowed(true, proactiveUsage{Today: MaxProactivePerChatPerDay}, now); ok {
			t.Fatal("третья инициатива за сутки должна быть запрещена")
		} else if reason == "" {
			t.Fatal("отмена должна иметь причину")
		}
	})
	t.Run("пауза 4 часа", func(t *testing.T) {
		last := now.Add(-3 * time.Hour)
		if ok, _ := ProactiveAllowed(true, proactiveUsage{Today: 1, Last: &last}, now); ok {
			t.Fatal("инициатива раньше 4 часов должна быть запрещена")
		}
		lastOk := now.Add(-4*time.Hour - time.Minute)
		if ok, _ := ProactiveAllowed(true, proactiveUsage{Today: 1, Last: &lastOk}, now); !ok {
			t.Fatal("после 4 часов инициатива разрешена")
		}
	})
}

// TestNextDelayJitter — пауза такта лежит в границах 15–30 с (§14.2).
func TestNextDelayJitter(t *testing.T) {
	s := &Scheduler{
		opts: Options{MinInterval: DefaultMinInterval, MaxInterval: DefaultMaxInterval}.withDefaults(),
		rnd:  rand.New(rand.NewSource(7)),
	}
	for i := 0; i < 200; i++ {
		d := s.nextDelay()
		if d < s.opts.MinInterval || d > s.opts.MaxInterval {
			t.Fatalf("пауза %v вне [%v, %v]", d, s.opts.MinInterval, s.opts.MaxInterval)
		}
	}
}

// TestParseConsolidationProposal — валидация proposal-контракта (§10.1):
// строгий JSON, длины, сжатие strength/interest в границы 0..1.
func TestParseConsolidationProposal(t *testing.T) {
	t.Run("валидный", func(t *testing.T) {
		raw := []byte(`{"summary":"Обсудили пластинки.","stances":[{"entity":"Коля","aspect":"вкус","summary":"Ценитель винила","strength":1.4,"interest":-0.2}]}`)
		p, err := parseConsolidationProposal(raw)
		if err != nil {
			t.Fatalf("валидный proposal отклонён: %v", err)
		}
		if p.Summary != "Обсудили пластинки." {
			t.Fatalf("summary потерялся: %q", p.Summary)
		}
		if len(p.Stances) != 1 {
			t.Fatalf("stances потерялись: %+v", p.Stances)
		}
		// Модель предлагает — runtime сжимает в границы (паритет сжатия дельт).
		if p.Stances[0].Strength != 1 || p.Stances[0].Interest != 0 {
			t.Fatalf("сжатие не сработало: %+v", p.Stances[0])
		}
		if p.Stances[0].Aspect != "вкус" {
			t.Fatalf("аспект потерялся: %q", p.Stances[0].Aspect)
		}
	})
	t.Run("пустой аспект становится general", func(t *testing.T) {
		p, err := parseConsolidationProposal([]byte(`{"summary":"x","stances":[{"entity":"E","aspect":"","summary":"s","strength":0.5,"interest":0.5}]}`))
		if err != nil || p.Stances[0].Aspect != "general" {
			t.Fatalf("aspect=general не подставился: %+v err=%v", p, err)
		}
	})
	t.Run("постороннее поле отклоняется", func(t *testing.T) {
		if _, err := parseConsolidationProposal([]byte(`{"summary":"x","confidence":0.9}`)); err == nil {
			t.Fatal("лишнее поле должно отклонять proposal (§10.1)")
		}
	})
	t.Run("пустое summary отклоняется", func(t *testing.T) {
		if _, err := parseConsolidationProposal([]byte(`{"summary":"  "}`)); err == nil {
			t.Fatal("пустое summary должно отклонять proposal")
		}
	})
	t.Run("мусор после JSON отклоняется", func(t *testing.T) {
		if _, err := parseConsolidationProposal([]byte(`{"summary":"x"} лишний текст`)); err == nil {
			t.Fatal("текст после JSON должен отклонять proposal")
		}
	})
	t.Run("слишком много stances отклоняется", func(t *testing.T) {
		stances := make([]map[string]any, maxStanceAspects+1)
		for i := range stances {
			stances[i] = map[string]any{"entity": "E", "aspect": "a", "summary": "s", "strength": 0.5, "interest": 0.5}
		}
		raw, _ := json.Marshal(map[string]any{"summary": "x", "stances": stances})
		if _, err := parseConsolidationProposal(raw); err == nil {
			t.Fatalf("больше %d stances должно отклонять", maxStanceAspects)
		}
	})
	t.Run("entity_merges принимаются и валидируются", func(t *testing.T) {
		raw := []byte(`{"summary":"x","entity_merges":[{"keep":"Коля","merge":"Николай","reason":"одно лицо"}]}`)
		p, err := parseConsolidationProposal(raw)
		if err != nil {
			t.Fatalf("валидный entity_merges отклонён: %v", err)
		}
		if len(p.EntityMerges) != 1 || p.EntityMerges[0].Keep != "Коля" ||
			p.EntityMerges[0].Merge != "Николай" || p.EntityMerges[0].Reason != "одно лицо" {
			t.Fatalf("entity_merges потерялись: %+v", p.EntityMerges)
		}
	})
	t.Run("entity_merges: пустая причина отклоняется", func(t *testing.T) {
		if _, err := parseConsolidationProposal([]byte(`{"summary":"x","entity_merges":[{"keep":"A","merge":"B","reason":"  "}]}`)); err == nil {
			t.Fatal("склейка без основания должна отклоняться")
		}
	})
	t.Run("entity_merges: слишком много отклоняется", func(t *testing.T) {
		merges := make([]map[string]any, maxEntityMerges+1)
		for i := range merges {
			merges[i] = map[string]any{"keep": "A", "merge": "B", "reason": "r"}
		}
		raw, _ := json.Marshal(map[string]any{"summary": "x", "entity_merges": merges})
		if _, err := parseConsolidationProposal(raw); err == nil {
			t.Fatalf("больше %d entity_merges должно отклонять", maxEntityMerges)
		}
	})
}
