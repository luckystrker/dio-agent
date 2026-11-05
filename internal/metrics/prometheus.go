// Prometheus text exposition (этап 31.1) — свой минимальный writer:
// внешняя зависимость prometheus/client_golang ради десятков gauge не
// оправдана (философия №6, минимализм). Формат — версия 0.0.4.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// sample — одна метрика с метками (label-пары идут парами key, value).
type sample struct {
	value  float64
	labels []string
}

// family — семейство метрик: имя, тип и выборки.
type family struct {
	name    string
	typ     string
	samples []sample
}

// WritePrometheus пишет снимок в формате Prometheus text exposition.
func WritePrometheus(w io.Writer, snap Snapshot) error {
	var b strings.Builder
	for _, f := range buildFamilies(snap) {
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.name, f.typ)
		for _, s := range f.samples {
			b.WriteString(f.name)
			if len(s.labels) > 0 {
				b.WriteString("{")
				for i := 0; i+1 < len(s.labels); i += 2 {
					if i > 0 {
						b.WriteString(",")
					}
					b.WriteString(s.labels[i])
					b.WriteString(`="`)
					b.WriteString(escapeLabel(s.labels[i+1]))
					b.WriteString(`"`)
				}
				b.WriteString("}")
			}
			b.WriteString(" ")
			b.WriteString(formatFloat(s.value))
			b.WriteString("\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func buildFamilies(snap Snapshot) []family {
	fams := make([]family, 0, 20)

	// Решения answer/react/ignore/defer за 1ч/24ч/7д.
	fams = append(fams, family{name: "dio_decisions_total", typ: "gauge", samples: []sample{
		{float64(snap.Decisions.Answer.Hour1), []string{"decision", "answer", "window", "1h"}},
		{float64(snap.Decisions.Answer.Day1), []string{"decision", "answer", "window", "24h"}},
		{float64(snap.Decisions.Answer.Day7), []string{"decision", "answer", "window", "7d"}},
		{float64(snap.Decisions.React.Hour1), []string{"decision", "react", "window", "1h"}},
		{float64(snap.Decisions.React.Day1), []string{"decision", "react", "window", "24h"}},
		{float64(snap.Decisions.React.Day7), []string{"decision", "react", "window", "7d"}},
		{float64(snap.Decisions.Ignore.Hour1), []string{"decision", "ignore", "window", "1h"}},
		{float64(snap.Decisions.Ignore.Day1), []string{"decision", "ignore", "window", "24h"}},
		{float64(snap.Decisions.Ignore.Day7), []string{"decision", "ignore", "window", "7d"}},
		{float64(snap.Decisions.Defer.Hour1), []string{"decision", "defer", "window", "1h"}},
		{float64(snap.Decisions.Defer.Day1), []string{"decision", "defer", "window", "24h"}},
		{float64(snap.Decisions.Defer.Day7), []string{"decision", "defer", "window", "7d"}},
	}})

	// Ходы по статусам и ретраи.
	turnSamples := make([]sample, 0, len(snap.Turns.ByStatus))
	for _, status := range sortedKeys(snap.Turns.ByStatus) {
		turnSamples = append(turnSamples, sample{float64(snap.Turns.ByStatus[status]), []string{"status", status}})
	}
	fams = append(fams,
		family{name: "dio_turns", typ: "gauge", samples: turnSamples},
		family{name: "dio_turns_created_24h", typ: "gauge", samples: []sample{{float64(snap.Turns.Created24h), nil}}},
		family{name: "dio_turns_retried_24h", typ: "gauge", samples: []sample{{float64(snap.Turns.Retried24h), nil}}},
	)
	skipSamples := make([]sample, 0, len(snap.Turns.Skipped24h))
	for _, skip := range snap.Turns.Skipped24h {
		skipSamples = append(skipSamples, sample{float64(skip.Count), []string{"phase", skip.Phase, "reason", skip.Reason}})
	}
	fams = append(fams, family{name: "dio_turn_skipped_total", typ: "gauge", samples: skipSamples})

	// Модель по пулам: вызовы, резервы, токены, лимит, остаток.
	pools := sortedKeys(snap.Model.Pools)
	calls := make([]sample, 0, len(pools))
	reserved := make([]sample, 0, len(pools))
	tokens := make([]sample, 0, len(pools))
	limits := make([]sample, 0, len(pools))
	remaining := make([]sample, 0, len(pools))
	for _, pool := range pools {
		p := snap.Model.Pools[pool]
		label := []string{"pool", pool}
		calls = append(calls, sample{float64(p.Calls), label})
		reserved = append(reserved, sample{float64(p.Reserved), label})
		tokens = append(tokens, sample{float64(p.Tokens), label})
		limits = append(limits, sample{float64(p.Limit), label})
		remaining = append(remaining, sample{float64(p.Remaining), label})
	}
	fams = append(fams,
		family{name: "dio_model_calls", typ: "gauge", samples: calls},
		family{name: "dio_model_calls_reserved", typ: "gauge", samples: reserved},
		family{name: "dio_model_tokens", typ: "gauge", samples: tokens},
		family{name: "dio_model_calls_limit", typ: "gauge", samples: limits},
		family{name: "dio_model_calls_remaining", typ: "gauge", samples: remaining},
	)

	// Инструменты памяти: доля ходов и причины включения.
	reasons := sortedKeys(snap.MemoryTools.Reasons)
	reasonSamples := make([]sample, 0, len(reasons))
	for _, reason := range reasons {
		reasonSamples = append(reasonSamples, sample{float64(snap.MemoryTools.Reasons[reason]), []string{"reason", reason}})
	}
	fams = append(fams,
		family{name: "dio_memory_turns_24h", typ: "gauge", samples: []sample{{float64(snap.MemoryTools.ConversationTurns24h), nil}}},
		family{name: "dio_memory_turns_with_tools_24h", typ: "gauge", samples: []sample{{float64(snap.MemoryTools.TurnsWithTools24h), nil}}},
		family{name: "dio_memory_tools_share", typ: "gauge", samples: []sample{{snap.MemoryTools.Share, nil}}},
		family{name: "dio_memory_tools_total", typ: "gauge", samples: reasonSamples},
	)

	// Время до первого bubble.
	fams = append(fams,
		family{name: "dio_first_bubble_ms", typ: "gauge", samples: []sample{
			{snap.FirstBubble.P50Ms, []string{"quantile", "0.5"}},
			{snap.FirstBubble.P95Ms, []string{"quantile", "0.95"}},
		}},
		family{name: "dio_first_bubble_count", typ: "gauge", samples: []sample{{float64(snap.FirstBubble.Count), nil}}},
	)

	// Очередь.
	fams = append(fams,
		family{name: "dio_queue_turns", typ: "gauge", samples: []sample{{float64(snap.Queue.TurnsCreated), nil}}},
		family{name: "dio_queue_oldest_turn_age_seconds", typ: "gauge", samples: []sample{{snap.Queue.OldestTurnAgeSeconds, nil}}},
		family{name: "dio_queue_outbox_pending", typ: "gauge", samples: []sample{{float64(snap.Queue.OutboxPending), nil}}},
		family{name: "dio_queue_intentions_due", typ: "gauge", samples: []sample{{float64(snap.Queue.IntentionsDue), nil}}},
	)

	// Доставки по статусам.
	fams = append(fams,
		family{name: "dio_deliveries_total", typ: "gauge", samples: deliverySamples(snap.Deliveries.Total)},
		family{name: "dio_deliveries_24h", typ: "gauge", samples: deliverySamples(snap.Deliveries.Last24h)},
		family{name: "dio_deliveries_replies_24h", typ: "gauge", samples: []sample{{float64(snap.Deliveries.RepliesSent24h), nil}}},
		family{name: "dio_deliveries_forwards_24h", typ: "gauge", samples: []sample{{float64(snap.Deliveries.ForwardsSent24h), nil}}},
	)

	// Инициативы по видам и лимиты §14.3.
	kinds := sortedKeys(snap.Intentions.ByKind)
	created := make([]sample, 0, len(kinds))
	completed := make([]sample, 0, len(kinds))
	cancelled := make([]sample, 0, len(kinds))
	failed := make([]sample, 0, len(kinds))
	pending := make([]sample, 0, len(kinds))
	for _, kind := range kinds {
		k := snap.Intentions.ByKind[kind]
		label := []string{"kind", kind}
		created = append(created, sample{float64(k.Created24h), label})
		completed = append(completed, sample{float64(k.Completed24h), label})
		cancelled = append(cancelled, sample{float64(k.Cancelled24h), label})
		failed = append(failed, sample{float64(k.Failed24h), label})
		pending = append(pending, sample{float64(k.Pending), label})
	}
	fams = append(fams,
		family{name: "dio_intentions_created_24h", typ: "gauge", samples: created},
		family{name: "dio_intentions_completed_24h", typ: "gauge", samples: completed},
		family{name: "dio_intentions_cancelled_24h", typ: "gauge", samples: cancelled},
		family{name: "dio_intentions_failed_24h", typ: "gauge", samples: failed},
		family{name: "dio_intentions_pending", typ: "gauge", samples: pending},
	)
	used := make([]sample, 0, len(snap.Intentions.ProactiveByChat))
	proactiveLeft := make([]sample, 0, len(snap.Intentions.ProactiveByChat))
	for _, chat := range snap.Intentions.ProactiveByChat {
		label := []string{"chat_id", strconv.FormatInt(chat.ChatID, 10)}
		used = append(used, sample{float64(chat.UsedToday), label})
		proactiveLeft = append(proactiveLeft, sample{float64(chat.Remaining), label})
	}
	fams = append(fams,
		family{name: "dio_proactive_used_today", typ: "gauge", samples: used},
		family{name: "dio_proactive_remaining", typ: "gauge", samples: proactiveLeft},
		family{name: "dio_proactive_limit_per_chat_per_day", typ: "gauge", samples: []sample{{float64(snap.Intentions.ProactiveLimitPerChatPerDay), nil}}},
		family{name: "dio_proactive_min_gap_minutes", typ: "gauge", samples: []sample{{float64(snap.Intentions.MinGapMinutes), nil}}},
		family{name: "dio_explorations_today", typ: "gauge", samples: []sample{{float64(snap.Intentions.ExplorationsToday), nil}}},
		family{name: "dio_explorations_limit", typ: "gauge", samples: []sample{{float64(snap.Intentions.ExplorationsLimit), nil}}},
	)

	// Скука и настроение.
	fams = append(fams,
		family{name: "dio_character_mood", typ: "gauge", samples: []sample{
			{snap.Character.Mood.Valence, []string{"scale", "valence"}},
			{snap.Character.Mood.Arousal, []string{"scale", "arousal"}},
			{snap.Character.Mood.SocialEnergy, []string{"scale", "social_energy"}},
		}},
		family{name: "dio_character_boredom", typ: "gauge", samples: []sample{{snap.Character.Boredom, nil}}},
	)

	// Стикеры, события жизни, память.
	fams = append(fams,
		family{name: "dio_stickers", typ: "gauge", samples: []sample{
			{float64(snap.Stickers.Persona), []string{"source", "persona"}},
			{float64(snap.Stickers.Saved), []string{"source", "saved"}},
		}},
		family{name: "dio_stickers_collection", typ: "gauge", samples: []sample{{float64(snap.Stickers.Collection), nil}}},
		family{name: "dio_stickers_sent_24h", typ: "gauge", samples: []sample{{float64(snap.Stickers.Sent24h), nil}}},
		family{name: "dio_life_events_24h", typ: "gauge", samples: []sample{{float64(snap.LifeEvents.Last24h), nil}}},
		family{name: "dio_memory_items", typ: "gauge", samples: []sample{{float64(snap.Memory.Active), []string{"status", "active"}}}},
		family{name: "dio_memory_items_with_vectors", typ: "gauge", samples: []sample{{float64(snap.Memory.WithVectors), nil}}},
	)

	// Веб разговорного пула и картинки (этап 36.2).
	fams = append(fams,
		family{name: "dio_web_searches_today", typ: "gauge", samples: []sample{{float64(snap.Web.SearchesToday), nil}}},
		family{name: "dio_web_searches_limit", typ: "gauge", samples: []sample{{float64(snap.Web.SearchesLimit), nil}}},
		family{name: "dio_images_sent_24h", typ: "gauge", samples: []sample{{float64(snap.Web.ImagesSent24h), nil}}},
	)

	// Состояние самого сервера метрик.
	fams = append(fams,
		family{name: "dio_metrics_scrape_errors_total", typ: "counter", samples: []sample{{float64(snap.Server.ScrapeErrors), nil}}},
		family{name: "dio_metrics_cache_age_seconds", typ: "gauge", samples: []sample{{snap.Server.CacheAgeSeconds, nil}}},
		family{name: "dio_metrics_uptime_seconds", typ: "gauge", samples: []sample{{snap.Server.UptimeSeconds, nil}}},
	)
	return fams
}

func deliverySamples(counts DeliveryStatusCounts) []sample {
	return []sample{
		{float64(counts.Sent), []string{"status", "sent"}},
		{float64(counts.Unknown), []string{"status", "unknown"}},
		{float64(counts.Failed), []string{"status", "failed"}},
		{float64(counts.Cancelled), []string{"status", "cancelled"}},
		{float64(counts.Pending), []string{"status", "pending"}},
		{float64(counts.RetryWait), []string{"status", "retry_wait"}},
		{float64(counts.Sending), []string{"status", "sending"}},
	}
}

// escapeLabel экранирует значение метки по правилам Prometheus.
func escapeLabel(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

// formatFloat печатает значение без лишних нулей (1200, а не 1200.000000).
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// sortedKeys — детерминированный порядок меток (стабильный вывод скрейпа).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
