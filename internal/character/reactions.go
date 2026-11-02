// Допустимые эмодзи реакций Telegram (ReactionTypeEmoji, Bot API на
// 2026-11-02). Telegram принимает фиксированный список: мусор вроде ": "
// доходит до setMessageReaction как 400 REACTION_INVALID, а доставка
// становится failed навсегда (bug 2026-11-02). Список — единственный источник
// истины и для валидатора, и для enum в JSON-схеме TurnResult.
package character

import "strings"

// telegramReactions — документированные формы ReactionTypeEmoji. Порядок — как
// в Bot API (для детерминированного enum в схеме).
var telegramReactions = []string{
	"❤", "👍", "👎", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱",
	"🤬", "😢", "🎉", "🤩", "🤮", "💩", "🙏", "👌", "🕊", "🤡",
	"🥱", "🥴", "😍", "🐳", "❤‍🔥", "🌚", "🌭", "💯", "🤣", "⚡",
	"🍌", "🏆", "💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈",
	"😴", "😭", "🤓", "👻", "👨‍💻", "👀", "🎃", "🙈", "😇", "😨",
	"🤝", "✍", "🤗", "🫡", "🎅", "🎄", "☃", "💅", "🤪", "🗿",
	"🆒", "💘", "🙉", "🦄", "😘", "💊", "🙊", "😎", "👾", "🤷‍♂",
	"🤷", "🤷‍♀", "😡",
}

// reactionVS16Variants — варианты с U+FE0F (emoji presentation) для эмодзи,
// чья документированная форма — текстовая. Оба варианта допустимы: ❤ и ❤️.
var reactionVS16Variants = map[string]string{
	"❤":   "❤️",
	"🕊":   "🕊️",
	"✍":   "✍️",
	"☃":   "☃️",
	"❤‍🔥": "❤️‍🔥",
	"🤷‍♂": "🤷‍♂️",
	"🤷‍♀": "🤷‍♀️",
}

var telegramReactionSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(telegramReactions))
	for _, e := range telegramReactions {
		set[e] = struct{}{}
	}
	return set
}()

// reactionEnum — значения enum JSON-схемы: документированные формы и их
// VS16-варианты (❤/❤️).
func reactionEnum() []string {
	out := make([]string, 0, len(telegramReactions)+len(reactionVS16Variants))
	out = append(out, telegramReactions...)
	for _, e := range telegramReactions {
		if variant, ok := reactionVS16Variants[e]; ok {
			out = append(out, variant)
		}
	}
	return out
}

// normalizeReaction убирает пробелы и вариационный селектор U+FE0F: Telegram
// принимает «❤» (документированная форма), а модель часто отдаёт «❤️».
func normalizeReaction(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\uFE0F", "")
}

// validReaction — эмодзи входит в ReactionTypeEmoji (после нормализации).
func validReaction(s string) bool {
	if s == "" {
		return false
	}
	_, ok := telegramReactionSet[normalizeReaction(s)]
	return ok
}
