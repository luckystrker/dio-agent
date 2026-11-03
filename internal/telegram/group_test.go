// Юнит-тесты повода в группе (30.1): имя без @, вопрос ко всем, косинусная
// близость темы, нормализация регистра/ё и границы слова.
package telegram

import (
	"context"
	"testing"

	"github.com/luckystrker/dio-agent/internal/memory"
)

func TestMentionsNameWholeWordAndYo(t *testing.T) {
	gate := NewGroupGate(nil, GroupGateConfig{Name: "Дио", Aliases: []string{"дийо"}})
	cases := []struct {
		text string
		want bool
	}{
		{"Привет, Дио!", true},
		{"дио, ты тут?", true},
		{"дийо привет", true},   // алиас
		{"радио играет", false}, // имя внутри слова
		{"диодный мост", false}, // граница слова
		{"Дионисий", false},     // имя — префикс чужого слова
		{"", false},
	}
	for _, c := range cases {
		if got := gate.mentionsName(c.text); got != c.want {
			t.Errorf("mentionsName(%q)=%v, хочу %v", c.text, got, c.want)
		}
	}
}

func TestAsksWholeChat(t *testing.T) {
	for _, text := range []string{
		"кто знает, где тут кофе?",
		"Есть кто живой?",
		"может кто подскажет",
		"кто-нибудь знает телефон мастера",
	} {
		if !asksWholeChat(text) {
			t.Errorf("вопрос ко всем не распознан: %q", text)
		}
	}
	for _, text := range []string{
		"я знаю, где кофе",
		"вчера был дождь",
		"Дио, ты тут?",
	} {
		if asksWholeChat(text) {
			t.Errorf("обычная реплика принята за вопрос ко всем: %q", text)
		}
	}
}

func TestTopicCatchesWithEmbedder(t *testing.T) {
	gate := NewGroupGate(nil, GroupGateConfig{
		Name:     "Дио",
		Topics:   []string{"старые пластинки и треск"},
		Embedder: memory.NewHashEmbedder(),
	})
	gate.Prepare(context.Background())
	if len(gate.topicVectors) == 0 {
		t.Fatalf("эмбеддинги тем не подготовлены")
	}
	if !gate.topicCatches(context.Background(), "старые пластинки и треск") {
		t.Fatalf("точное совпадение темы не зацепило")
	}
	if gate.topicCatches(context.Background(), "налоги отчётность бухгалтерия") {
		t.Fatalf("несвязанный текст зацепил тему")
	}
}

func TestGroupGateWithoutEmbedderUsesNameOnly(t *testing.T) {
	gate := NewGroupGate(nil, GroupGateConfig{Name: "Дио", Topics: []string{"старые пластинки"}})
	gate.Prepare(context.Background())
	if gate.topicCatches(context.Background(), "старые пластинки") {
		t.Fatalf("без векторизатора тема не должна цеплять")
	}
}
