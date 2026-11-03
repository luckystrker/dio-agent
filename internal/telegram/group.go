// Группы (этап 30): повод прочитать пачку моделью и вовлечённость чата.
//
// 30.1. Пачка пассивной группы закрывается своим окном (3–5/6 с), и только
// тогда дешёвый фильтр решает: прямое обращение, имя бота без @, вопрос ко
// всем или «цепляющая тема» (эмбеддинг пачки близок к интересам/темам
// персоны) — пачка идёт в модель; иначе journal_only и остаётся в истории.
// Эмбеддинг — сетевой вызов, поэтому решение принимается вне ingest-пути.
// Встревания без прямого обращения ограничены: не чаще раза в 20 минут на
// группу.
//
// 30.2. После того как бот ответил или поставил реакцию, чат «вовлечён»:
// 5 минут каждая пачка идёт в модель, любое сообщение продлевает окно, но не
// дальше потолка 30 минут подряд без прямого обращения; прямое обращение
// сбрасывает потолок. Состояние — в chat_involvement и переживает рестарт.
package telegram

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// Константы групп (начальные настраиваемые значения, этап 30).
const (
	// GroupInvolvementWindow — сколько живёт вовлечённость после ответа бота;
	// каждое сообщение продлевает её снова на это окно.
	GroupInvolvementWindow = 5 * time.Minute
	// GroupInvolvementCeiling — потолок подряд идущей вовлечённости без
	// прямого обращения к боту.
	GroupInvolvementCeiling = 30 * time.Minute
	// GroupUnpromptedCooldown — потолок «встреваний» без прямого обращения:
	// не чаще раза в 20 минут на группу.
	GroupUnpromptedCooldown = 20 * time.Minute
	// groupTopicDistanceMax — порог близости темы пачки к интересам персоны
	// (калибровка bge-m3, как у карточки памяти: 0.45).
	groupTopicDistanceMax = 0.45
	// groupTriggerTextLimit — потолок текста пачки для эмбеддинга.
	groupTriggerTextLimit = 1000
)

// BatchDecision — решение о судьбе закрытой пачки.
type BatchDecision struct {
	// Process — пачка идёт в ход; false — события остаются в журнале и
	// недавней истории (journal_only).
	Process bool
	// Reason — причина для лога (наблюдаемость §20).
	Reason string
}

// BatchGate — дешёвый фильтр закрытой пачки (30.1/30.2). Ошибка означает
// «решение не принято»: события остаются pending, их подберёт recovery.
type BatchGate interface {
	DecideBatch(ctx context.Context, chatID int64, ids []int64) (BatchDecision, error)
}

// groupDB — минимум запросов к БД для гейта и вовлечённости; реализуется
// пулом PostgreSQL и транзакцией.
type groupDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// GroupGateConfig — настройки гейта из персоны.
type GroupGateConfig struct {
	// Name — имя персоны («Имя» persona-файла); Aliases — необязательные
	// алиасы: «дио», «дийо» и т.п.
	Name    string
	Aliases []string
	// Topics — интересы и темы для любопытства: кэш их эмбеддингов строится
	// при старте (Prepare).
	Topics []string
	// Embedder — векторизатор EMBEDDING_*; nil — работают только имя и
	// шаблоны вопросов (семантика честно недоступна).
	Embedder memory.Embedder
	Logger   *log.Logger
	Now      func() time.Time
}

// GroupGate — реализация BatchGate для групп.
type GroupGate struct {
	cfg  GroupGateConfig
	db   groupDB
	log  *log.Logger
	now  func() time.Time
	name []string
	// topicVectors — кэш эмбеддингов тем персоны; пусто — семантика выключена
	// или не подготовлена.
	topicVectors [][]float32
}

// NewGroupGate строит гейт; Prepare кэширует эмбеддинги тем.
func NewGroupGate(db groupDB, cfg GroupGateConfig) *GroupGate {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(discardWriter{}, "", 0)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	names := make([]string, 0, len(cfg.Aliases)+1)
	if strings.TrimSpace(cfg.Name) != "" {
		names = append(names, cfg.Name)
	}
	for _, alias := range cfg.Aliases {
		if strings.TrimSpace(alias) != "" {
			names = append(names, alias)
		}
	}
	return &GroupGate{cfg: cfg, db: db, log: logger, now: now, name: names}
}

// Prepare кэширует эмбеддинги тем персоны (30.1: «кэш эмбеддингов тем при
// старте»). Сбой/отсутствие векторизатора — работают только имя и шаблоны.
func (g *GroupGate) Prepare(ctx context.Context) {
	if g == nil || g.cfg.Embedder == nil || len(g.cfg.Topics) == 0 {
		return
	}
	vectors, err := g.cfg.Embedder.Embed(ctx, g.cfg.Topics)
	if err != nil {
		g.log.Printf("группы: эмбеддинги тем не подготовлены, повод по теме выключен: %v", err)
		return
	}
	g.topicVectors = vectors
}

// DecideBatch решает судьбу пачки группы (30.1/30.2). Для лички — всегда ход.
func (g *GroupGate) DecideBatch(ctx context.Context, chatID int64, ids []int64) (BatchDecision, error) {
	if len(ids) == 0 {
		return BatchDecision{Process: false, Reason: "пустая пачка"}, nil
	}
	db := g.db
	if db == nil {
		return BatchDecision{}, fmt.Errorf("группы: гейт без БД")
	}

	events, chatType, err := loadBatchSignals(ctx, db, ids)
	if err != nil {
		return BatchDecision{}, err
	}
	if chatType == "" || chatType == string(ChatPrivate) {
		return BatchDecision{Process: true, Reason: "личка"}, nil
	}

	now := g.now()
	direct := false
	for _, ev := range events {
		if ev.direct {
			direct = true
			break
		}
	}
	if direct {
		if err := ResetGroupCeiling(ctx, db, chatID, now); err != nil {
			return BatchDecision{}, err
		}
		return BatchDecision{Process: true, Reason: "прямое обращение"}, nil
	}

	involved, err := GroupInvolved(ctx, db, chatID, now)
	if err != nil {
		return BatchDecision{}, err
	}
	if involved {
		if err := ExtendGroupInvolvement(ctx, db, chatID, now); err != nil {
			return BatchDecision{}, err
		}
		return BatchDecision{Process: true, Reason: "чат вовлечён"}, nil
	}

	text := batchText(events)
	if g.mentionsName(text) {
		// Имя без @ — обращение к боту: потолок сбрасывается, пачка идёт в ход.
		if err := ResetGroupCeiling(ctx, db, chatID, now); err != nil {
			return BatchDecision{}, err
		}
		return BatchDecision{Process: true, Reason: "имя"}, nil
	}

	// Дальше — только «встревания» без прямого обращения: потолок 20 минут.
	allowed, err := GroupUnpromptedAllowed(ctx, db, chatID, now)
	if err != nil {
		return BatchDecision{}, err
	}
	if !allowed {
		return BatchDecision{Process: false, Reason: "потолок встреваний (20 мин)"}, nil
	}
	if asksWholeChat(text) {
		if err := TouchGroupUnprompted(ctx, db, chatID, now); err != nil {
			return BatchDecision{}, err
		}
		return BatchDecision{Process: true, Reason: "вопрос ко всем"}, nil
	}
	if g.topicCatches(ctx, text) {
		if err := TouchGroupUnprompted(ctx, db, chatID, now); err != nil {
			return BatchDecision{}, err
		}
		return BatchDecision{Process: true, Reason: "цепляющая тема"}, nil
	}
	return BatchDecision{Process: false, Reason: "пассивный шум"}, nil
}

// mentionsName — в тексте есть имя персоны или алиас целым словом (регистр и
// ё/е нормализуются; «радио» не содержит имя «дио»).
func (g *GroupGate) mentionsName(text string) bool {
	norm := normalizeMatchText(text)
	for _, name := range g.name {
		needle := normalizeMatchText(name)
		if needle != "" && containsWord(norm, needle) {
			return true
		}
	}
	return false
}

// topicCatches — эмбеддинг пачки близок к интересам/темам персоны (30.1).
func (g *GroupGate) topicCatches(ctx context.Context, text string) bool {
	if g.cfg.Embedder == nil || len(g.topicVectors) == 0 {
		return false
	}
	query := strings.TrimSpace(text)
	if query == "" {
		return false
	}
	if runes := []rune(query); len(runes) > groupTriggerTextLimit {
		query = string(runes[:groupTriggerTextLimit])
	}
	vectors, err := g.cfg.Embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) == 0 {
		g.log.Printf("группы: эмбеддинг пачки не удался, повод по теме пропущен: %v", err)
		return false
	}
	best := math.Inf(1)
	for _, topic := range g.topicVectors {
		if d := cosineDistance(vectors[0], topic); d < best {
			best = d
		}
	}
	if best <= groupTopicDistanceMax {
		g.log.Printf("группы: цепляющая тема (дистанция %.3f)", best)
		return true
	}
	return false
}

// batchSignal — минимум события для гейта.
type batchSignal struct {
	text   string
	direct bool
}

// loadBatchSignals читает тексты/direct пачки и тип чата.
func loadBatchSignals(ctx context.Context, db groupDB, ids []int64) ([]batchSignal, string, error) {
	rows, err := db.Query(ctx, `
		SELECT coalesce(text, ''), direct, chat_type
		FROM social_events WHERE id = ANY($1::bigint[]) ORDER BY id`, ids)
	if err != nil {
		return nil, "", fmt.Errorf("группы: пачка не читается: %w", err)
	}
	defer rows.Close()
	var out []batchSignal
	chatType := ""
	for rows.Next() {
		var ev batchSignal
		var ct string
		if err := rows.Scan(&ev.text, &ev.direct, &ct); err != nil {
			return nil, "", fmt.Errorf("группы: строка пачки не читается: %w", err)
		}
		if chatType == "" {
			chatType = ct
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("группы: пачка не дочитана: %w", err)
	}
	return out, chatType, nil
}

// batchText склеивает тексты пачки в один запрос.
func batchText(events []batchSignal) string {
	parts := make([]string, 0, len(events))
	for _, ev := range events {
		if text := strings.TrimSpace(ev.text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

// normalizeMatchText — нижний регистр и ё→е: простая нормализация словоформ
// имён («Дио», «диё»).
func normalizeMatchText(text string) string {
	return strings.ReplaceAll(strings.ToLower(text), "ё", "е")
}

// containsWord — needle входит в text на границах слова (буквы/цифры).
func containsWord(text, needle string) bool {
	if needle == "" {
		return false
	}
	for from := 0; from <= len(text)-len(needle); {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(needle)
		leftOK := start == 0 || !isWordRune(runeBefore(text, start))
		rightOK := end == len(text) || !isWordRune(runeAfter(text, end))
		if leftOK && rightOK {
			return true
		}
		from = start + 1
	}
	return false
}

// runeBefore — последняя руна префикса (0 на границе).
func runeBefore(text string, end int) rune {
	r, _ := utf8.DecodeLastRuneInString(text[:end])
	return r
}

// runeAfter — первая руна суффикса (0 на границе).
func runeAfter(text string, start int) rune {
	r, _ := utf8.DecodeRuneInString(text[start:])
	return r
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// groupQuestionPatterns — простые шаблоны вопроса, адресованного всем (30.1).
var groupQuestionPatterns = []string{
	"кто знает",
	"кто-нибудь знает",
	"кто-то знает",
	"кто подскажет",
	"кто в курсе",
	"кто может",
	"кто умеет",
	"есть кто",
	"есть тут кто",
	"может кто",
	"кто из вас",
}

// asksWholeChat — текст задаёт вопрос всему чату, а не конкретному человеку.
func asksWholeChat(text string) bool {
	norm := normalizeMatchText(text)
	for _, pattern := range groupQuestionPatterns {
		if strings.Contains(norm, pattern) {
			return true
		}
	}
	return false
}

// cosineDistance — 1 − косинус; 1 для нулевого/разноразмерного вектора.
func cosineDistance(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}
