// Юнит-тесты prefilter §6.3: прямой запрос не отбрасывается, шум группы —
// journal_only. Поведение — src/telegram/prefilter.ts (+ prefilter.pbt.test.ts).
package telegram

import "testing"

func TestDecideIngestStatus(t *testing.T) {
	cases := []struct {
		name  string
		event NormalizedEvent
		want  IngestStatus
	}{
		{"личка — всегда прямое общение",
			NormalizedEvent{Supported: true, ChatType: ChatPrivate, Kind: KindMessage}, StatusPending},
		{"личка — даже правка",
			NormalizedEvent{Supported: true, ChatType: ChatPrivate, Kind: KindEdited}, StatusPending},
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideIngestStatus(tc.event); got != tc.want {
				t.Fatalf("статус = %s, хочу %s", got, tc.want)
			}
		})
	}
}
