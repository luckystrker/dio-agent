// Снимок метрик §20 (этап 31.2) — единая структура для /stats (JSON) и
// /metrics (Prometheus text exposition). Источник истины — PostgreSQL
// (философия №2): снимок собирается SQL-запросами, поэтому переживает рестарт
// процесса; счётчики в памяти приложения описывают только сам сервер метрик.
package metrics

import "time"

// Snapshot — согласованный набор метрик на момент сбора.
type Snapshot struct {
	GeneratedAt time.Time `json:"generated_at"`

	Decisions   Decisions        `json:"decisions"`
	Turns       TurnsStats       `json:"turns"`
	Model       ModelStats       `json:"model"`
	MemoryTools MemoryToolsStats `json:"memory_tools"`
	FirstBubble LatencyStats     `json:"first_bubble"`
	Queue       QueueStats       `json:"queue"`
	Deliveries  Deliveries       `json:"deliveries"`
	Intentions  IntentionsStats  `json:"intentions"`
	Character   CharacterStats   `json:"character"`
	Stickers    StickersStats    `json:"stickers"`
	LifeEvents  LifeEventsStats  `json:"life_events"`
	Memory      MemoryStats      `json:"memory"`

	// Server заполняет сам HTTP-сервер: это не состояние базы.
	Server ServerStats `json:"server"`
}

// Decisions — решения модели (распределение answer/react/ignore/defer, §20).
type Decisions struct {
	Answer DecisionCounts `json:"answer"`
	React  DecisionCounts `json:"react"`
	Ignore DecisionCounts `json:"ignore"`
	Defer  DecisionCounts `json:"defer"`
}

// DecisionCounts — одно решение в трёх окнах.
type DecisionCounts struct {
	Hour1 int64 `json:"1h"`
	Day1  int64 `json:"24h"`
	Day7  int64 `json:"7d"`
}

// TurnsStats — ходы по статусам, пропуски и ретраи.
type TurnsStats struct {
	ByStatus   map[string]int64 `json:"by_status"`
	Created24h int64            `json:"created_24h"`
	Retried24h int64            `json:"retried_24h"`
	Skipped24h []SkipCount      `json:"skipped_24h"`
}

// SkipCount — терминальные пропуски (turn_skipped) по причине за сутки.
// Причина обрезана до первой строки: в ней бывают сырые ответы модели (21.7).
type SkipCount struct {
	Phase  string `json:"phase"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// ModelStats — модельные вызовы и токены по пулам за текущие сутки (§16, §20)
// и дневной лимит пула.
type ModelStats struct {
	Period string               `json:"period"`
	Pools  map[string]ModelPool `json:"pools"`
}

// ModelPool — расход одного пула: completed/reserved вызовы, токены, лимит.
type ModelPool struct {
	Calls     int64 `json:"calls"`
	Reserved  int64 `json:"reserved_calls"`
	Tokens    int64 `json:"tokens"`
	Limit     int64 `json:"limit"`
	Remaining int64 `json:"remaining"`
}

// MemoryToolsStats — инструменты памяти на разговорных ходах за сутки:
// доля ходов с инструментами и причины включения (журнал хода, не логи).
type MemoryToolsStats struct {
	ConversationTurns24h int64            `json:"conversation_turns_24h"`
	TurnsWithTools24h    int64            `json:"turns_with_tools_24h"`
	Share                float64          `json:"share"`
	Reasons              map[string]int64 `json:"reasons_24h"`
}

// LatencyStats — время до первого bubble за сутки (от создания хода до sent
// первой строки outbox): p50/p95 в миллисекундах.
type LatencyStats struct {
	Count int64   `json:"count"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
}

// QueueStats — очередь работы: ходы в очереди, возраст старейшего, outbox
// pending и due-намерения.
type QueueStats struct {
	TurnsCreated         int64   `json:"turns_created"`
	OldestTurnAgeSeconds float64 `json:"oldest_turn_age_seconds"`
	OutboxPending        int64   `json:"outbox_pending"`
	IntentionsDue        int64   `json:"intentions_due"`
}

// Deliveries — доставки по статусам: всего и изменившиеся за сутки.
type Deliveries struct {
	Total   DeliveryStatusCounts `json:"total"`
	Last24h DeliveryStatusCounts `json:"last_24h"`
}

// DeliveryStatusCounts — счётчики статусов delivery_outbox (§13.2).
type DeliveryStatusCounts struct {
	Sent      int64 `json:"sent"`
	Unknown   int64 `json:"unknown"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	Pending   int64 `json:"pending"`
	RetryWait int64 `json:"retry_wait"`
	Sending   int64 `json:"sending"`
}

// IntentionsStats — намерения по видам и остаток проактивных лимитов §14.3.
type IntentionsStats struct {
	ByKind                      map[string]IntentionKindCounts `json:"by_kind"`
	ProactiveLimitPerChatPerDay int                            `json:"proactive_limit_per_chat_per_day"`
	MinGapMinutes               int                            `json:"min_gap_minutes"`
	ProactiveByChat             []ProactiveChat                `json:"proactive_by_chat"`
	ExplorationsToday           int64                          `json:"explorations_today"`
	ExplorationsLimit           int                            `json:"explorations_limit"`
}

// IntentionKindCounts — создано/завершено/отменено за сутки и живые сейчас.
type IntentionKindCounts struct {
	Created24h   int64 `json:"created_24h"`
	Completed24h int64 `json:"completed_24h"`
	Cancelled24h int64 `json:"cancelled_24h"`
	Failed24h    int64 `json:"failed_24h"`
	Pending      int64 `json:"pending"`
}

// ProactiveChat — использованный остаток инициатив личного чата (§14.3).
type ProactiveChat struct {
	ChatID    int64 `json:"chat_id"`
	UsedToday int64 `json:"used_today"`
	Remaining int64 `json:"remaining"`
}

// CharacterStats — текущее настроение и скука (затухание учтено).
type CharacterStats struct {
	Mood    MoodStats `json:"mood"`
	Boredom float64   `json:"boredom"`
}

// MoodStats — три шкалы настроения (§7.2).
type MoodStats struct {
	Valence      float64 `json:"valence"`
	Arousal      float64 `json:"arousal"`
	SocialEnergy float64 `json:"social_energy"`
}

// StickersStats — коллекция стикеров и отправки за сутки (этап 25).
type StickersStats struct {
	Collection int64 `json:"collection"`
	Persona    int64 `json:"persona"`
	Saved      int64 `json:"saved"`
	Sent24h    int64 `json:"sent_24h"`
}

// LifeEventsStats — вымышленные события жизни персонажа (этап 24).
type LifeEventsStats struct {
	Last24h int64 `json:"last_24h"`
}

// MemoryStats — активные записи памяти и покрытие векторами (§10.4).
type MemoryStats struct {
	Active      int64 `json:"active"`
	WithVectors int64 `json:"with_vectors"`
}

// ServerStats — состояние самого сервера метрик (единственные счётчики
// процесса: их нет и не должно быть в базе).
type ServerStats struct {
	ScrapeErrors    int64   `json:"scrape_errors"`
	CacheAgeSeconds float64 `json:"cache_age_seconds"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
}
