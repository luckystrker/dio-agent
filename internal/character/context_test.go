// Юнит-тесты сборки текстов пачки (без БД): пустые тексты помечаются
// плейсхолдером, фото без подписи — как фото (находка приёмки Р: модель
// получала «сообщение без текста» рядом с приложенной картинкой).
package character

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBatchTexts(t *testing.T) {
	photoRaw := json.RawMessage(`{"update_id":1,"message":{"message_id":2,
		"photo":[{"file_id":"thumb","width":90,"height":90},{"file_id":"big","width":1280,"height":720}]}}`)
	textRaw := json.RawMessage(`{"update_id":3,"message":{"message_id":4,"text":"привет"}}`)

	batch := []BatchEvent{
		{ActorID: 7, Text: "смотри какой закат", Raw: textRaw},
		{ActorID: 7, Raw: photoRaw}, // фото без подписи
		{ActorID: 7},                // и правда без текста
	}
	got := batchTexts(batch)
	if len(got) != 3 {
		t.Fatalf("текстов %d, хочу 3: %v", len(got), got)
	}
	if got[0] != "смотри какой закат" {
		t.Fatalf("текст потерян: %q", got[0])
	}
	if got[1] != "[фото без подписи]" {
		t.Fatalf("фото не помечено: %q", got[1])
	}
	if got[2] != "[сообщение без текста]" {
		t.Fatalf("пустое не помечено: %q", got[2])
	}
	if strings.Contains(got[1], "file_id") {
		t.Fatalf("сырой update не должен утекать в текст: %q", got[1])
	}
}
