// Инициативы в группы (этап 30.3): редко и по делу.
//
// share может уйти в группу, где за последние GroupShareLiveWindow был живой
// разговор с ботом (бот писал, человек ответил), тема находки ближе к недавней
// истории группы, чем к личкам, и не больше одного раза в сутки на группу.
// check_in в группу — никогда; reply_later — только туда, где создан (это его
// target_chat_id, перенацеливания нет). Тихие часы уже отсекает quietDecision.
// Приватность §9.1: provenance намерения обязан разрешать аудиторию группы —
// share, основанный на приватной памяти лички, в группу не уходит.
package background

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/luckystrker/dio-agent/internal/memory"
)

const (
	// GroupShareLiveWindow — насколько недавним должен быть живой разговор с
	// ботом в группе, чтобы share был уместен.
	GroupShareLiveWindow = 6 * time.Hour
	// GroupSharePerDay — потолок share в группу за сутки (reply_later — 41.2 M6
	// — дневным потолком не ограничивается).
	GroupSharePerDay = 1
	// groupShareTopicDistanceMax — абсолютный порог близости темы к истории
	// группы (калибровка bge-m3, как у карточки памяти).
	groupShareTopicDistanceMax = 0.45
	// groupHistoryLimit — сколько последних реплик каждой стороны участвует в
	// сравнении темы.
	groupHistoryLimit = 30
)

// groupInitiativeAllowed — политика инициативы в групповой чат (§14.3, 30.3).
// false — отменить с причиной; error — проверка не удалась (транзиентный
// сбой эмбеддинга/БД), намерение откладывается, а не теряется.
func (s *Scheduler) groupInitiativeAllowed(ctx context.Context, it Intention, now time.Time) (bool, string, error) {
	switch it.Kind {
	case KindCheckIn:
		return false, "check_in в группу не отправляется (§14.3)", nil
	case KindShare:
		ok, reason, err := s.groupShareAllowed(ctx, it, now)
		return ok, reason, err
	case KindReplyLater:
		// Reply_later создан в этой группе (defer в ответ на сообщение) и
		// остаётся там, где создан. 41.2 M6: это отложенный ответ на прямое
		// обращение, а не проактивная инициатива — групповой дневной потолок
		// share к нему не применяется, только пауза между инициативами.
		usage, err := s.intentions.ProactiveUsage(ctx, it.TargetChatID)
		if err != nil {
			return false, "", err
		}
		if allowed, reason := proactiveGapAllowed(usage, now); !allowed {
			return false, reason, nil
		}
		return true, "", nil
	default:
		return false, "инициативы в группы выключены (§14.3)", nil
	}
}

// groupShareAllowed — условия share в группу (30.3).
func (s *Scheduler) groupShareAllowed(ctx context.Context, it Intention, now time.Time) (bool, string, error) {
	// §9.1: источник находки должен разрешать аудиторию группы. Иначе share
	// из лички, основанный на приватной памяти, утёк бы в группу.
	scopeAllowed, err := s.intentionScopeAllowsChat(ctx, it, it.TargetChatID)
	if err != nil {
		return false, "", err
	}
	if !scopeAllowed {
		return false, "share основан на приватной памяти другого чата — в группу нельзя (§9.1)", nil
	}

	live, err := s.groupConversationLive(ctx, it.TargetChatID, now.Add(-GroupShareLiveWindow))
	if err != nil {
		return false, "", err
	}
	if !live {
		return false, fmt.Sprintf("в группе не было живого разговора за %v (§14.3)", GroupShareLiveWindow), nil
	}

	usage, err := s.intentions.ProactiveUsage(ctx, it.TargetChatID)
	if err != nil {
		return false, "", err
	}
	if usage.Today >= GroupSharePerDay {
		return false, fmt.Sprintf("дневной лимит инициатив группы (%d/сутки, §14.3)", GroupSharePerDay), nil
	}

	embedder := s.embedder()
	if embedder == nil {
		return false, "семантика недоступна — тема share к группе не проверена (§14.3)", nil
	}
	closer, err := s.topicCloserToGroup(ctx, it.TargetChatID, it.Topic, embedder)
	if err != nil {
		return false, "", err
	}
	if !closer {
		return false, "тема share не ближе группе, чем личкам (§14.3)", nil
	}
	return true, "", nil
}

// embedder — векторизатор инструментария памяти; nil — семантика выключена.
func (s *Scheduler) embedder() memory.Embedder {
	if s.memory == nil {
		return nil
	}
	return s.memory.Embedder()
}

// groupConversationLive — за окно в группе бот писал, и человек ответил после
// его сообщения (для share «живой разговор», а не разовая реплика в пустоту).
func (s *Scheduler) groupConversationLive(ctx context.Context, chatID int64, since time.Time) (bool, error) {
	var live bool
	err := s.pool.QueryRow(ctx, `
		WITH bot AS (
		  SELECT max(created_at) AS at
		  FROM delivery_outbox
		  WHERE chat_id = $1 AND status = 'sent' AND created_at > $2
		    AND payload->>'kind' IN ('message', 'sticker')
		)
		SELECT coalesce((SELECT at FROM bot) IS NOT NULL
		  AND EXISTS (
		    SELECT 1 FROM social_events e
		    WHERE e.chat_id = $1 AND e.chat_type = 'group'
		      AND e.occurred_at >= (SELECT at FROM bot)
		      AND e.occurred_at > $2), false)`, chatID, since).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("фон: живой разговор группы %d не проверяется: %w", chatID, err)
	}
	return live, nil
}

// topicCloserToGroup — эмбеддинг темы ближе к недавней истории группы, чем к
// личкам, и не дальше абсолютного порога. Без истории группы сравнивать
// нечего — false.
func (s *Scheduler) topicCloserToGroup(ctx context.Context, chatID int64, topic string, embedder memory.Embedder) (bool, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return false, nil
	}
	groupTexts, err := s.historyTexts(ctx, chatID, true)
	if err != nil {
		return false, err
	}
	if len(groupTexts) == 0 {
		return false, nil
	}
	privateTexts, err := s.historyTexts(ctx, chatID, false)
	if err != nil {
		return false, err
	}

	texts := make([]string, 0, 1+len(groupTexts)+len(privateTexts))
	texts = append(texts, topic)
	texts = append(texts, groupTexts...)
	texts = append(texts, privateTexts...)
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil || len(vectors) != len(texts) {
		return false, fmt.Errorf("фон: эмбеддинг темы share не удался: %w", err)
	}
	topicVec := vectors[0]
	groupDist := minDistance(topicVec, vectors[1:1+len(groupTexts)])
	if math.IsInf(groupDist, 1) || groupDist > groupShareTopicDistanceMax {
		return false, nil
	}
	if len(privateTexts) == 0 {
		return true, nil
	}
	privateDist := minDistance(topicVec, vectors[1+len(groupTexts):])
	return groupDist < privateDist, nil
}

// historyTexts — последние реплики недавней истории: группы chatID или всех
// личек. Пустые тексты не участвуют.
func (s *Scheduler) historyTexts(ctx context.Context, chatID int64, group bool) ([]string, error) {
	query := `
		SELECT text FROM social_events
		WHERE text IS NOT NULL AND btrim(text) <> ''
		  AND occurred_at > now() - $1::interval
		  AND chat_type = 'private'
		ORDER BY occurred_at DESC LIMIT $2`
	args := []any{GroupShareLiveWindow, groupHistoryLimit}
	if group {
		query = `
			SELECT text FROM social_events
			WHERE text IS NOT NULL AND btrim(text) <> ''
			  AND occurred_at > now() - $1::interval
			  AND chat_id = $2
			ORDER BY occurred_at DESC LIMIT $3`
		args = []any{GroupShareLiveWindow, chatID, groupHistoryLimit}
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("фон: история для темы share не читается: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("фон: строка истории для темы share не читается: %w", err)
		}
		out = append(out, text)
	}
	return out, rows.Err()
}

// intentionScopeAllowsChat — provenance намерения разрешает аудиторию чата
// (§9.1): share из лички не уходит в группу.
func (s *Scheduler) intentionScopeAllowsChat(ctx context.Context, it Intention, chatID int64) (bool, error) {
	var audiences []string
	err := s.pool.QueryRow(ctx, `
		SELECT p.scopes FROM intentions i JOIN provenance p ON p.id = i.provenance_id
		WHERE i.id = $1`, it.ID).Scan(&audiences)
	if err != nil {
		return false, fmt.Errorf("фон: разрешение намерения %d не читается: %w", it.ID, err)
	}
	want := memory.ChatScope(fmt.Sprintf("%d", chatID))
	for _, audience := range audiences {
		if audience == want {
			return true, nil
		}
	}
	return false, nil
}

// minDistance — минимальная косинусная дистанция темы до текстов.
func minDistance(topic []float32, vectors [][]float32) float64 {
	best := math.Inf(1)
	for _, v := range vectors {
		if d := cosineDistance(topic, v); d < best {
			best = d
		}
	}
	return best
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
