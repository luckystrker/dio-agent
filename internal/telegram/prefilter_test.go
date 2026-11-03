// Юнит-тесты prefilter §6.3: прямой запрос не отбрасывается, шум группы —
// journal_only. Поведение — src/telegram/prefilter.ts (+ prefilter.pbt.test.ts).
package telegram

import "testing"

func TestNormalizeReplyAndMentionOnlyToBot(t *testing.T) {
	bot := BotIdentity{ID: 900, Username: "dio_bot"}

	// Reply на сообщение другого человека в группе — не обращение к боту.
	other := []byte(`{"update_id":1,"message":{"message_id":10,"chat":{"id":-5,"type":"group"},` +
		`"from":{"id":1,"first_name":"Аня"},"text":"смотри","reply_to_message":{"message_id":9,"from":{"id":2,"first_name":"Боря"},"text":"раньше"}}}`)
	ev, err := NormalizeUpdateWithBot(other, bot)
	if err != nil {
		t.Fatalf("reply чужому: %v", err)
	}
	if ev.Kind != KindReply || ev.ReplyToBot || !ev.BotKnown {
		t.Fatalf("reply чужому распознан как обращение к боту: %+v", ev)
	}

	// Reply на сообщение бота — обращение.
	toBot := []byte(`{"update_id":2,"message":{"message_id":11,"chat":{"id":-5,"type":"group"},` +
		`"from":{"id":1,"first_name":"Аня"},"text":"смотри","reply_to_message":{"message_id":8,"from":{"id":900,"first_name":"Dio"},"text":"ответ"}}}`)
	ev, err = NormalizeUpdateWithBot(toBot, bot)
	if err != nil {
		t.Fatalf("reply боту: %v", err)
	}
	if !ev.ReplyToBot {
		t.Fatalf("reply боту не распознан: %+v", ev)
	}

	// Mention чужого — не обращение, mention бота — обращение.
	mentionOther := []byte(`{"update_id":3,"message":{"message_id":12,"chat":{"id":-5,"type":"group"},` +
		`"from":{"id":1,"first_name":"Аня"},"text":"@other привет","entities":[{"type":"mention","offset":0,"length":6}]}}`)
	ev, err = NormalizeUpdateWithBot(mentionOther, bot)
	if err != nil {
		t.Fatalf("mention чужого: %v", err)
	}
	if ev.Kind != KindMention || ev.MentionBot {
		t.Fatalf("mention чужого распознан как бот: %+v", ev)
	}

	mentionBot := []byte(`{"update_id":4,"message":{"message_id":13,"chat":{"id":-5,"type":"group"},` +
		`"from":{"id":1,"first_name":"Аня"},"text":"@Dio_Bot привет","entities":[{"type":"mention","offset":0,"length":8}]}}`)
	ev, err = NormalizeUpdateWithBot(mentionBot, bot)
	if err != nil {
		t.Fatalf("mention бота: %v", err)
	}
	if !ev.MentionBot {
		t.Fatalf("mention бота не распознан: %+v", ev)
	}

	// Без идентичности бота флаги ложные, prefilter — по прежним правилам.
	ev, err = NormalizeUpdate(other)
	if err != nil {
		t.Fatalf("без getMe: %v", err)
	}
	if ev.BotKnown || ev.ReplyToBot || ev.MentionBot {
		t.Fatalf("без идентичности бота выставлены флаги: %+v", ev)
	}
}

func TestDecideIngestStatus(t *testing.T) {
	cases := []struct {
		name  string
		event NormalizedEvent
		want  IngestStatus
	}{
		{"личка — всегда прямое общение",
			NormalizedEvent{Supported: true, ChatType: ChatPrivate, Kind: KindMessage}, StatusPending},
		{"личка — правка не запускает новый ход (minor 13)",
			NormalizedEvent{Supported: true, ChatType: ChatPrivate, Kind: KindEdited}, StatusJournalOnly},
		{"reply в группе — прямое обращение",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindReply}, StatusPending},
		{"mention в группе — прямое обращение",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindMention}, StatusPending},
		{"command в группе — прямое обращение",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindCommand}, StatusPending},
		{"шум группы — журнал",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindMessage}, StatusJournalOnly},
		{"правка в группе — журнал",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindEdited}, StatusJournalOnly},
		{"unsupported — журнал (offset не застревает)",
			NormalizedEvent{Supported: false, ChatType: ChatGroup, Kind: KindOther}, StatusJournalOnly},
		{"reply чужому в группе — журнал (M8)",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindReply, BotKnown: true, ReplyToBot: false}, StatusJournalOnly},
		{"reply боту в группе — обращение (M8)",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindReply, BotKnown: true, ReplyToBot: true}, StatusPending},
		{"mention чужого в группе — журнал (M8)",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindMention, BotKnown: true, MentionBot: false}, StatusJournalOnly},
		{"mention бота в группе — обращение (M8)",
			NormalizedEvent{Supported: true, ChatType: ChatGroup, Kind: KindMention, BotKnown: true, MentionBot: true}, StatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideIngestStatus(tc.event); got != tc.want {
				t.Fatalf("статус = %s, хочу %s", got, tc.want)
			}
		})
	}
}
