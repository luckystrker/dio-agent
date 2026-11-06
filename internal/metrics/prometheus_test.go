// Тесты Prometheus-экспозиции (этап 31.1): свой минимальный writer вместо
// внешней зависимости (философия №6), поэтому формат проверяется явно —
// имена метрик, метки и экранирование.
package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrometheusExposition(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheus(&buf, sampleSnapshot()); err != nil {
		t.Fatalf("экспозиция не собрана: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`dio_decisions_total{decision="answer",window="1h"} 3`,
		`dio_decisions_total{decision="defer",window="7d"} 2`,
		`dio_turns{status="applied"} 10`,
		`dio_turns_created_24h 5`,
		`dio_turns_retried_24h 1`,
		`dio_turn_skipped_total{phase="generate",reason="модель не настроена"} 2`,
		`dio_model_calls{pool="conversation"} 10`,
		`dio_model_tokens{pool="conversation"} 4000`,
		`dio_model_calls_limit{pool="conversation"} 100`,
		`dio_model_calls_remaining{pool="conversation"} 88`,
		`dio_model_prefix_cache_share_24h 0.75`,
		`dio_model_prefix_cache_samples_24h 6`,
		`dio_memory_turns_24h 10`,
		`dio_memory_turns_with_tools_24h 3`,
		`dio_memory_tools_share 0.3`,
		`dio_memory_tools_total{reason="card_incomplete"} 3`,
		`dio_first_bubble_ms{quantile="0.5"} 1200`,
		`dio_first_bubble_ms{quantile="0.95"} 5000`,
		`dio_first_bubble_count 4`,
		`dio_queue_turns 2`,
		`dio_queue_oldest_turn_age_seconds 35`,
		`dio_queue_outbox_pending 1`,
		`dio_queue_intentions_due 0`,
		`dio_deliveries_total{status="sent"} 10`,
		`dio_deliveries_total{status="unknown"} 1`,
		`dio_deliveries_24h{status="sent"} 4`,
		`dio_deliveries_replies_24h 0`,
		`dio_deliveries_forwards_24h 0`,
		`dio_intentions_created_24h{kind="share"} 2`,
		`dio_intentions_completed_24h{kind="share"} 1`,
		`dio_proactive_used_today{chat_id="777"} 1`,
		`dio_proactive_remaining{chat_id="777"} 1`,
		`dio_proactive_limit_per_chat_per_day 2`,
		`dio_explorations_today 0`,
		`dio_explorations_limit 2`,
		`dio_character_mood{scale="valence"} 0.2`,
		`dio_character_mood{scale="arousal"} 0.5`,
		`dio_character_mood{scale="social_energy"} 0.6`,
		`dio_character_boredom 0.4`,
		`dio_stickers{source="persona"} 4`,
		`dio_stickers{source="saved"} 1`,
		`dio_stickers_collection 5`,
		`dio_stickers_sent_24h 2`,
		`dio_life_events_24h 1`,
		`dio_memory_items{status="active"} 20`,
		`dio_memory_items_with_vectors 18`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("в экспозиции нет строки %q", want)
		}
	}

	// Каждое семейство объявлено ровно одним TYPE — иначе Prometheus ругается
	// на дубли.
	if n := strings.Count(out, "# TYPE dio_decisions_total gauge"); n != 1 {
		t.Errorf("# TYPE dio_decisions_total встречается %d раз, хочу 1", n)
	}
	if !strings.Contains(out, "# TYPE dio_model_calls gauge") {
		t.Errorf("нет объявления типа dio_model_calls")
	}
}

func TestPrometheusEscaping(t *testing.T) {
	snap := sampleSnapshot()
	snap.Turns.Skipped24h = []SkipCount{{Phase: "generate", Reason: "ответ \"битый\"\n строка", Count: 1}}

	var buf bytes.Buffer
	if err := WritePrometheus(&buf, snap); err != nil {
		t.Fatalf("экспозиция не собрана: %v", err)
	}
	out := buf.String()
	want := `dio_turn_skipped_total{phase="generate",reason="ответ \"битый\"\n строка"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("метка не экранирована; хочу строку %q в выводе:\n%s", want, out)
	}
}

func TestPrometheusNoTrailingGarbage(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheus(&buf, Snapshot{}); err != nil {
		t.Fatalf("пустой снимок не сериализуется: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			if strings.Count(line, " ") != 3 {
				t.Errorf("строка TYPE неверного вида: %q", line)
			}
			continue
		}
		if !strings.Contains(line, " ") {
			t.Errorf("у метрики нет значения: %q", line)
		}
	}
}
