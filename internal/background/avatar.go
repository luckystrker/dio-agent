// Аватарка собеседника (34.2): фоновая проверка после разговора, отдельный
// дешёвый модельный вызов с картинкой, запись памяти в скоупе чата (§9.2).
// Ответ не задерживается: проверка ставится в очередь ходом (character.apply)
// и выполняется в такте фона; модель узнаёт об аватарке со следующего хода.
package background

import (
	"context"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	agomiddleware "github.com/luckystrker/ago/middleware"

	"github.com/luckystrker/dio-agent/internal/avatar"
	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/modebug"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// AvatarSource — Bot API для аватарок (getUserProfilePhotos + getFile).
// Реализация — internal/telegram.AvatarFetcher; nil — проверки не запускаются.
type AvatarSource interface {
	ProfilePhoto(ctx context.Context, userID int64) (fileID, fileUniqueID string, err error)
	FetchAvatar(ctx context.Context, fileID string) (mime string, data []byte, err error)
}

// avatarChecksPerTick — сколько проверок за такт: аватарка дешёвая, но такт
// фона не должен превращаться в пачку модельных вызовов.
const avatarChecksPerTick = 2

// avatarSystemPrompt — отдельный дешёвый вызов видит только картинку: без
// персоны, истории и инструментов.
const avatarSystemPrompt = "Ты помогаешь персонажу запомнить собеседника. По аватарке опиши одной короткой фразой, " +
	"как её видит человек: что на ней и какое настроение. Без оценок личности, без выдумок и без обращения к собеседнику. " +
	"Ответ — только эта фраза."

// maybeCheckAvatars — такт фоновой проверки аватарок (34.2). Ошибка одной
// проверки не валит остальные и не роняет такт.
func (s *Scheduler) maybeCheckAvatars(ctx context.Context) error {
	if s.avatar == nil {
		return nil
	}
	checks, err := avatar.ClaimDue(ctx, s.pool, avatarChecksPerTick)
	if err != nil {
		return err
	}
	for _, check := range checks {
		if err := s.checkAvatar(ctx, check); err != nil {
			s.logError("аватарка пользователя %d (чат %d): %v", check.UserID, check.ChatID, err)
			if ferr := avatar.Fail(ctx, s.pool, check.UserID, check.ScopeID); ferr != nil {
				s.logError("аватарка пользователя %d: отложенная попытка не записана: %v", check.UserID, ferr)
			}
			continue
		}
	}
	return nil
}

// checkAvatar — одна проверка: нет аватарки → «не видно» один раз; та же
// file_unique_id → ничего; новая/сменилась → скачать, описать, записать память.
func (s *Scheduler) checkAvatar(ctx context.Context, check avatar.Check) error {
	fileID, uniqueID, err := s.avatar.ProfilePhoto(ctx, check.UserID)
	if err != nil {
		return fmt.Errorf("getUserProfilePhotos: %w", err)
	}
	if fileID == "" || uniqueID == "" {
		// Аватарки нет или она скрыта приватностью: пишем «не видно» один раз
		// (повтор по следующей проверке через сутки не дублирует запись).
		text := avatar.InvisibleText(check.ActorName)
		if check.Visible || check.Description == "" {
			if err := s.writeAvatarMemory(ctx, check, text, "avatar:"+fmt.Sprint(check.UserID)+":none"); err != nil {
				return err
			}
		}
		return avatar.Complete(ctx, s.pool, check.UserID, check.ScopeID, "", false, text)
	}
	if uniqueID == check.FileUniqueID {
		// Аватарка не менялась: память не трогаем, следующая проверка — сутки.
		return avatar.Complete(ctx, s.pool, check.UserID, check.ScopeID, uniqueID, true, check.Description)
	}

	mime, data, err := s.avatar.FetchAvatar(ctx, fileID)
	if err != nil {
		return fmt.Errorf("скачивание аватарки: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("аватарка пустая")
	}
	description, err := s.describeAvatar(ctx, check, mime, data, uniqueID)
	if err != nil {
		return err
	}
	text := avatar.DescriptionText(check.ActorName, description)
	if err := s.writeAvatarMemory(ctx, check, text,
		fmt.Sprintf("avatar:%d:%s", check.UserID, uniqueID)); err != nil {
		return err
	}
	return avatar.Complete(ctx, s.pool, check.UserID, check.ScopeID, uniqueID, true, description)
}

// writeAvatarMemory — запись памяти experience в скоупе чата (§9.2): источник
// и разрешение назначает runtime, запись привязана к карточке человека.
func (s *Scheduler) writeAvatarMemory(ctx context.Context, check avatar.Check, text, ref string) error {
	if _, err := (memory.EntityRepo{}).EnsurePerson(ctx, s.pool, check.UserID,
		check.ActorName, check.ScopeID); err != nil {
		return fmt.Errorf("карточка собеседника: %w", err)
	}
	source := memory.SourceRef{Kind: memory.SourceRuntimeAction, Ref: ref}
	if _, err := (memory.SourceRepo{}).EnsureSource(ctx, s.pool, source); err != nil {
		return fmt.Errorf("источник аватарки: %w", err)
	}
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, s.pool,
		memory.SourceRuntimeAction, ref, []string{check.ScopeID})
	if err != nil {
		return fmt.Errorf("разрешение аватарки: %w", err)
	}
	_, err = (memory.Items{}).Insert(ctx, s.pool, memory.NewItem{
		Kind:         "experience",
		Text:         text,
		Salience:     0.4,
		ProvenanceID: provenanceID,
		Source:       &source,
		Links: []memory.EntityLink{{
			Kind: memory.EntityPerson,
			Name: memory.PersonEntityName(check.UserID),
			Role: "subject",
		}},
	}, check.ScopeID)
	if err != nil {
		return fmt.Errorf("запись памяти об аватарке: %w", err)
	}
	return nil
}

// describeAvatar — отдельный дешёвый модельный вызов с картинкой (пул
// background): описание одной фразой, без схемы TurnResult и инструментов.
func (s *Scheduler) describeAvatar(ctx context.Context, check avatar.Check, mime string, data []byte, uniqueID string) (string, error) {
	cfg, err := s.model.Clone(map[string]any{"session_id": sessionForChat(check.ChatID)})
	if err != nil {
		return "", fmt.Errorf("конфиг модели не клонирован: %w", err)
	}
	counting := &usageCounter{}
	op := fmt.Sprintf("model:avatar:%d:%s", check.UserID, uniqueID)
	// Дамп модельных вызовов (40.1): описание аватарки — тоже вызов провайдера.
	ctx = modebug.WithCall(ctx, modebug.NewCall("bg", dumpTask(op)))
	a := agent.New("dio-avatar", avatarSystemPrompt,
		agent.WithConfig(cfg),
		agent.WithMiddleware(
			s.cognitionMiddleware,
			func() agomiddleware.Middleware {
				return character.NewBudgetMiddlewarePool(s.budget, op, s.dailyLimit,
					runtime.BudgetPoolBackground, s.logger)
			},
			func() agomiddleware.Middleware { return counting },
		),
	)
	image, err := events.ImageInput("", "", data, mime, "")
	if err != nil {
		return "", fmt.Errorf("картинка аватарки: %w", err)
	}
	userText := "Аватарка собеседника"
	if name := strings.TrimSpace(check.ActorName); name != "" {
		userText += " " + name
	}
	userText += ". Опиши одной фразой."
	reply, err := a.Ask(ctx, userText, image)
	if err != nil {
		return "", fmt.Errorf("модельный вызов аватарки: %w", err)
	}
	body, err := reply.Content()
	if err != nil {
		return "", fmt.Errorf("ответ модели по аватарке: %w", err)
	}
	description := strings.Join(strings.Fields(strings.TrimSpace(string(body))), " ")
	if description == "" {
		return "", fmt.Errorf("модель не описала аватарку")
	}
	runes := []rune(description)
	if len(runes) > avatar.DescriptionLimit {
		description = string(runes[:avatar.DescriptionLimit])
	}
	return description, nil
}
