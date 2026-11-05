// Приёмочные тесты этапа 13 (фиксы ручной приёмки Р): ожидания — из спеки и
// пунктов CHECKLIST 13.2–13.6, не из устройства кода. Отдельный файл, чтобы не
// смешивать с юнит-тестами модулей.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// acc13Marshal — тестовый update из карты: полный контроль над JSON без ручного
// экранирования строк.
func acc13Marshal(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("тестовый update не сериализуется: %v", err)
	}
	return raw
}

// acc13PrivateMessage — каркас личного сообщения от автора с именем.
func acc13PrivateMessage(extra map[string]any) map[string]any {
	msg := map[string]any{
		"message_id": 10,
		"from":       map[string]any{"id": 7, "first_name": "Оля"},
		"chat":       map[string]any{"id": 42, "type": "private"},
	}
	for k, v := range extra {
		msg[k] = v
	}
	return msg
}

// acc13Quoted — цель реплая: сообщение от Васи с переданным содержимым.
func acc13Quoted(extra map[string]any) map[string]any {
	q := map[string]any{
		"message_id": 5,
		"from":       map[string]any{"id": 3, "first_name": "Вася"},
		"chat":       map[string]any{"id": 42, "type": "private"},
	}
	for k, v := range extra {
		q[k] = v
	}
	return q
}

// acc13ReplyUpdate — update с сообщением-реплаем на цель.
func acc13ReplyUpdate(own, quoted map[string]any) map[string]any {
	return map[string]any{
		"update_id": 1,
		"message":   acc13PrivateMessage(map[string]any{"reply_to_message": acc13Quoted(quoted), "text": own["text"]}),
	}
}

// --- 13.2 Реплай: цитата цели в тексте события ---

// Реплай с автором цели: «[в ответ на Имя: «текст цели»] собственный текст».
func TestAcc13ReplyQuoteWithAuthor(t *testing.T) {
	raw := acc13Marshal(t, acc13ReplyUpdate(
		map[string]any{"text": "а вот это правда?"},
		map[string]any{"text": "никогда не сдавайся"},
	))
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация реплая упала: %v", err)
	}
	want := "[в ответ на Вася: «никогда не сдавайся»] а вот это правда?"
	if ev.Text == nil {
		t.Fatal("текст события пуст, ожидалась цитата + собственный текст")
	}
	if *ev.Text != want {
		t.Errorf("текст события = %q, ожидалось %q", *ev.Text, want)
	}
	if ev.Kind != KindReply {
		t.Errorf("kind = %q, ожидался %q", ev.Kind, KindReply)
	}
	if !ev.Supported {
		t.Error("реплай должен быть поддержан (Supported=true)")
	}
}

// Цель без автора: «[в ответ на «текст»]» — без имени, но с цитатой.
func TestAcc13ReplyQuoteNoAuthor(t *testing.T) {
	raw := acc13Marshal(t, acc13ReplyUpdate(
		map[string]any{"text": "соглашусь"},
		map[string]any{"from": nil, "text": "зато тепло"}, // from отсутствует
	))
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	if ev.Text == nil {
		t.Fatal("текст события пуст")
	}
	want := "[в ответ на «зато тепло»] соглашусь"
	if *ev.Text != want {
		t.Errorf("текст события = %q, ожидалось %q", *ev.Text, want)
	}
}

// Потолок цитаты — 200 рун с многоточием; ровно 200 рун — без многоточия.
func TestAcc13ReplyQuoteRuneLimit(t *testing.T) {
	long := strings.Repeat("ж", 250)
	exact := strings.Repeat("ж", 200)
	emoji := strings.Repeat("😊", 210) // руны, не байты
	cases := []struct {
		name    string
		quoted  string
		content string
	}{
		{"длиннее лимита — 200 рун и многоточие", long, "«" + strings.Repeat("ж", 200) + "…»"},
		{"ровно лимит — без многоточия", exact, "«" + exact + "»"},
		{"эмодзи считаются рунами", emoji, "«" + strings.Repeat("😊", 200) + "…»"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := acc13Marshal(t, acc13ReplyUpdate(
				map[string]any{"text": "ответ"},
				map[string]any{"text": tc.quoted},
			))
			ev, err := NormalizeUpdate(raw)
			if err != nil {
				t.Fatalf("нормализация упала: %v", err)
			}
			want := "[в ответ на Вася: " + tc.content + "] ответ"
			if ev.Text == nil || *ev.Text != want {
				t.Errorf("текст события = %v, ожидалось %q", ev.Text, want)
			}
		})
	}
}

// Честные пометки для целей без текста: фото, стикер, пустая цель.
func TestAcc13ReplyQuoteSpecialTargets(t *testing.T) {
	cases := []struct {
		name   string
		quoted map[string]any
		mark   string
	}{
		{"цель-фото", map[string]any{"photo": []map[string]any{{"file_id": "f1", "width": 100, "height": 80}}}, "(фото)"},
		{"цель-стикер", map[string]any{"sticker": map[string]any{"emoji": "🐱"}}, "(стикер)"},
		{"цель без содержимого", map[string]any{}, "(без текста)"},
		{"цель из одних пробелов", map[string]any{"text": "   "}, "(без текста)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := acc13Marshal(t, acc13ReplyUpdate(
				map[string]any{"text": "смотри"},
				tc.quoted,
			))
			ev, err := NormalizeUpdate(raw)
			if err != nil {
				t.Fatalf("нормализация упала: %v", err)
			}
			want := "[в ответ на Вася: " + tc.mark + "] смотри"
			if ev.Text == nil || *ev.Text != want {
				t.Errorf("текст события = %v, ожидалось %q", ev.Text, want)
			}
		})
	}
}

// Реплай без собственного текста — текст события не nil, только префикс.
func TestAcc13ReplyWithoutOwnTextNotNil(t *testing.T) {
	raw := acc13Marshal(t, map[string]any{
		"update_id": 1,
		"message":   acc13PrivateMessage(map[string]any{"reply_to_message": acc13Quoted(map[string]any{"text": "привет"})}),
	})
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	if ev.Text == nil {
		t.Fatal("текст события nil, ожидался хотя бы префикс цитаты")
	}
	if got := strings.TrimSpace(*ev.Text); got != "[в ответ на Вася: «привет»]" {
		t.Errorf("текст события = %q, ожидался один префикс цитаты", got)
	}
}

// Спецсимволы JSON в цитате доезжают как есть (без порчи при разборе).
func TestAcc13ReplyQuoteSpecialCharacters(t *testing.T) {
	tricky := "кавычки \" и бэкслеш \\ и <тег> & амперсант"
	raw := acc13Marshal(t, acc13ReplyUpdate(
		map[string]any{"text": "ок"},
		map[string]any{"text": tricky},
	))
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	want := "[в ответ на Вася: «" + tricky + "»] ок"
	if ev.Text == nil || *ev.Text != want {
		t.Errorf("текст события = %v, ожидалось %q", ev.Text, want)
	}
}

// --- 13.3 Стикеры ---

// Стикер с эмодзи: kind=sticker, поддержан, текст «[стикер ЭМОДЗИ]».
func TestAcc13StickerWithEmoji(t *testing.T) {
	raw := acc13Marshal(t, map[string]any{
		"update_id": 2,
		"message":   acc13PrivateMessage(map[string]any{"sticker": map[string]any{"emoji": "😂", "is_video": false}}),
	})
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация стикера упала: %v", err)
	}
	if string(KindSticker) != "sticker" {
		t.Fatalf("константа kind стикера = %q, ожидалась %q", string(KindSticker), "sticker")
	}
	if ev.Kind != KindSticker {
		t.Errorf("kind = %q, ожидался %q", ev.Kind, KindSticker)
	}
	if !ev.Supported {
		t.Error("стикер должен быть поддержан (Supported=true), а не «сообщение без текста»")
	}
	if ev.Text == nil || *ev.Text != "[стикер 😂]" {
		t.Errorf("текст события = %v, ожидался %q", ev.Text, "[стикер 😂]")
	}
}

// Стикер без эмодзи: поддержан и не превращается в «сообщение без текста» —
// честная пометка «[стикер]» (согласованное изменение спеки 13.3, итерация 2).
func TestAcc13StickerWithoutEmoji(t *testing.T) {
	raw := acc13Marshal(t, map[string]any{
		"update_id": 3,
		"message":   acc13PrivateMessage(map[string]any{"sticker": map[string]any{"is_video": true}}),
	})
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	if !ev.Supported {
		t.Error("стикер без эмодзи должен оставаться поддержанным")
	}
	if ev.Kind != KindSticker {
		t.Errorf("kind = %q, ожидался %q", ev.Kind, KindSticker)
	}
	if ev.Text == nil || *ev.Text != "[стикер]" {
		t.Errorf("текст события = %v, ожидался %q", ev.Text, "[стикер]")
	}
}

// Стикер с reply_to_message — это реплай (реплай старше стикера).
func TestAcc13StickerWithReplyIsKindReply(t *testing.T) {
	raw := acc13Marshal(t, map[string]any{
		"update_id": 4,
		"message": acc13PrivateMessage(map[string]any{
			"sticker":          map[string]any{"emoji": "😂"},
			"reply_to_message": acc13Quoted(map[string]any{"text": "хаха"}),
		}),
	})
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	if ev.Kind != KindReply {
		t.Errorf("kind = %q, ожидался %q (реплай старше стикера)", ev.Kind, KindReply)
	}
	if ev.Text == nil || *ev.Text != "[в ответ на Вася: «хаха»] [стикер 😂]" {
		t.Errorf("текст события = %v, ожидался префикс цитаты и эмодзи стикера", ev.Text)
	}
}

// --- 13.4 Реакция: payload setMessageReaction ---

// Клиент шлёт chat_id, message_id и reaction-массив [{"type":"emoji","emoji":…}];
// ключа «emoji» в payload быть не должно (Bot API его молча игнорировал).
func TestAcc13SetMessageReactionPayload(t *testing.T) {
	var gotPath, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("content-type")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	client := NewClient("tok", srv.URL, nil)
	if err := client.SetMessageReaction(context.Background(), 777, 42, "🔥"); err != nil {
		t.Fatalf("setMessageReaction упал: %v", err)
	}

	if gotPath != "/bottok/setMessageReaction" {
		t.Errorf("путь запроса = %q, ожидался %q", gotPath, "/bottok/setMessageReaction")
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, ожидался application/json", gotContentType)
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload не разбирается как JSON: %v (тело: %s)", err, gotBody)
	}
	if _, ok := payload["emoji"]; ok {
		t.Error("payload содержит ключ «emoji» — Bot API его игнорирует, реакции не будет")
	}
	chatID, ok := payload["chat_id"].(float64)
	if !ok || chatID != 777 {
		t.Errorf("chat_id = %v, ожидался 777", payload["chat_id"])
	}
	messageID, ok := payload["message_id"].(float64)
	if !ok || messageID != 42 {
		t.Errorf("message_id = %v, ожидался 42", payload["message_id"])
	}
	reaction, ok := payload["reaction"].([]any)
	if !ok {
		t.Fatalf("reaction не массив: %v", payload["reaction"])
	}
	if len(reaction) != 1 {
		t.Fatalf("в reaction %d элементов, ожидался 1", len(reaction))
	}
	item, ok := reaction[0].(map[string]any)
	if !ok {
		t.Fatalf("элемент reaction не объект: %v", reaction[0])
	}
	if item["type"] != "emoji" {
		t.Errorf("reaction[0].type = %v, ожидалось \"emoji\"", item["type"])
	}
	if item["emoji"] != "🔥" {
		t.Errorf("reaction[0].emoji = %v, ожидалось \"🔥\"", item["emoji"])
	}
}

// --- 13.5 Токен не течёт в журнал ---

// Ошибка транспорта (соединение закрыто) не содержит токен — вместо него «[токен]».
func TestAcc13TransportErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // порт закрыт → отказ соединения, URL с токеном попадает в url.Error

	client := NewClient("sekret-token-abc", srv.URL, nil)
	_, err := client.SendMessage(context.Background(), 1, "привет", 0)
	if err == nil {
		t.Fatal("ожидалась ошибка транспорта, получен успех")
	}
	if strings.Contains(err.Error(), "sekret-token-abc") {
		t.Errorf("текст ошибки содержит токен: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[токен]") {
		t.Errorf("в тексте ошибки ожидалось «[токен]»: %q", err.Error())
	}
}

// Затирание сохраняет классификацию доставки: errors.Is(err, context.DeadlineExceeded).
func TestAcc13RedactionKeepsDeadlineClassification(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// Имитация таймаута: url.Error от Client.Do добавит URL с токеном в текст.
		return nil, fmt.Errorf("post %s: %w", r.URL.String(), context.DeadlineExceeded)
	})
	client := NewClient("sekret-token-abc", "http://botapi.invalid", &http.Client{Transport: rt})
	_, err := client.SendMessage(context.Background(), 1, "привет", 0)
	if err == nil {
		t.Fatal("ожидалась ошибка, получен успех")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("классификация потеряна: errors.Is(err, context.DeadlineExceeded) = false")
	}
	if strings.Contains(err.Error(), "sekret-token-abc") {
		t.Errorf("текст ошибки содержит токен: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[токен]") {
		t.Errorf("в тексте ошибки ожидалось «[токен]»: %q", err.Error())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Скачивание файлов тоже затирает токен (тот же base с токеном в URL).
func TestAcc13DownloadFileHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	client := NewClient("sekret-token-abc", srv.URL, nil)

	t.Run("downloadFile", func(t *testing.T) {
		_, err := client.downloadFile(context.Background(), "photos/file_1.jpg")
		if err == nil {
			t.Fatal("ожидалась ошибка транспорта")
		}
		if strings.Contains(err.Error(), "sekret-token-abc") {
			t.Errorf("текст ошибки скачивания содержит токен: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "[токен]") {
			t.Errorf("в тексте ошибки скачивания ожидалось «[токен]»: %q", err.Error())
		}
	})

	t.Run("FetchPhotoByFileID", func(t *testing.T) {
		_, err := client.FetchPhotoByFileID(context.Background(), "file_x")
		if err == nil {
			t.Fatal("ожидалась ошибка getFile")
		}
		if strings.Contains(err.Error(), "sekret-token-abc") {
			t.Errorf("текст ошибки getFile содержит токен: %q", err.Error())
		}
	})
}

// Цитируемое фото с подписью: цитируется подпись как текст цели, а не «(фото)».
func TestAcc13ReplyQuotePhotoCaptionAsTargetText(t *testing.T) {
	raw := acc13Marshal(t, acc13ReplyUpdate(
		map[string]any{"text": "красиво"},
		map[string]any{
			"photo":   []map[string]any{{"file_id": "f1", "width": 100, "height": 80}},
			"caption": "закат в горах",
		},
	))
	ev, err := NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	want := "[в ответ на Вася: «закат в горах»] красиво"
	if ev.Text == nil || *ev.Text != want {
		t.Errorf("текст события = %v, ожидалось %q", ev.Text, want)
	}
}

// Токена нет в тексте ошибки — ошибка возвращается как есть, без обёртки затирания.
func TestAcc13RedactPassesThroughWithoutToken(t *testing.T) {
	client := NewClient("sekret-token-abc", "http://botapi.invalid", nil)
	plain := errors.New("соединение оборвалось посреди записи")
	got := client.redact(plain)
	if _, wrapped := got.(*redactedError); wrapped {
		t.Errorf("ошибка без токена в тексте завернулась в redactedError: %q", got.Error())
	}
	if got != plain {
		t.Errorf("ошибка без токена изменилась: %q → %q", plain.Error(), got.Error())
	}
}

// --- 13.6 tcp4-транспорт ---

// Проводка: NewClient с nil-клиентом обязан работать именно через
// defaultTransport. Мутация «конструктор вернул &http.Client{} без транспорта»
// не должна переживать набор: dial tcp6 на v4-адрес через транспорт,
// полученный конструктором, удаться только у tcp4-версии.
func TestAcc13NewClientWiringUsesDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("адрес листенера не разобран: %v", err)
	}

	client := NewClient("tok", srv.URL, nil)
	tr, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("клиент с nil-аргументом получил транспорт %T, ожидался *http.Transport из defaultTransport", client.http.Transport)
	}
	conn, err := tr.DialContext(context.Background(), "tcp6", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("транспорт конструктора не набирает tcp4 (dial tcp6 на %s упал): %v", net.JoinHostPort(host, port), err)
	}
	_ = conn.Close()
}

// Дефолтный транспорт набирает tcp4 независимо от запрошенной сети: dial «tcp6»
// на v4-адрес живого листенера удаться.
func TestAcc13DefaultTransportForcesTCP4(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("адрес листенера не разобран: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("листенер не на v4-адресе: %q", host)
	}

	tr := defaultTransport()
	for _, network := range []string{"tcp", "tcp6"} {
		t.Run("запрошена сеть "+network, func(t *testing.T) {
			conn, err := tr.DialContext(context.Background(), network, net.JoinHostPort(host, port))
			if err != nil {
				t.Fatalf("dial %s на %s не удался: %v", network, net.JoinHostPort(host, port), err)
			}
			defer conn.Close()
			// Соединение по v4-адресу состоялось вопреки tcp6-запросу; проверим
			// и семейство адреса (строки Network() на Windows всегда «tcp»).
			ra, ok := conn.RemoteAddr().(*net.TCPAddr)
			if !ok || ra.IP.To4() == nil {
				t.Errorf("соединение не по IPv4: %v", conn.RemoteAddr())
			}
		})
	}
}
