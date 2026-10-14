package bot

import (
	"context"
	"fmt"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/sqlc"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/service"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

// Inline result IDs: "d:<format>" starts a download, "c:<format>" was sent from cache.
const (
	inlineDownload = "d"
	inlineCached   = "c"
)

var inlineFormats = []struct {
	format ytdl.Format
	title  string
}{
	{ytdl.FormatMP3, "🎵 MP3 audio"},
	{ytdl.FormatMP4, "🎬 MP4 video (480p)"},
	{ytdl.FormatWAV, "🎼 WAV audio"},
}

func (h *Handler) onInlineQuery(ctx context.Context, b *tgbot.Bot, iq *models.InlineQuery, u sqlc.User) {
	results, err := h.inlineResults(ctx, iq, u)
	if err != nil {
		h.log.Error("inline query", "user_id", u.ID, "err", err)
		results = []models.InlineQueryResult{hint("error", "Something went wrong", "Please try again later.")}
	}
	if _, err := b.AnswerInlineQuery(ctx, &tgbot.AnswerInlineQueryParams{
		InlineQueryID: iq.ID,
		Results:       results,
		CacheTime:     5,
		IsPersonal:    true,
	}); err != nil {
		h.log.Warn("answer inline query", "err", err)
	}
}

func (h *Handler) inlineResults(ctx context.Context, iq *models.InlineQuery, u sqlc.User) ([]models.InlineQueryResult, error) {
	if u.Status != service.StatusActive {
		return []models.InlineQueryResult{hint("no-access", "No access yet",
			fmt.Sprintf("Open @%s and send /start to request access.", h.username))}, nil
	}
	if !h.svc.InlineEnabled() {
		return []models.InlineQueryResult{hint("disabled", "Inline mode is not configured",
			"Ask the admin to set STORAGE_CHAT_ID.")}, nil
	}
	link, ok := service.ParseURL(iq.Query)
	if !ok {
		return []models.InlineQueryResult{hint("usage", "Paste a link",
			fmt.Sprintf("Type @%s <link>, e.g. a YouTube URL.", h.username))}, nil
	}
	if remaining, limited, err := h.svc.Remaining(ctx, u); err != nil {
		return nil, err
	} else if limited && remaining == 0 {
		return []models.InlineQueryResult{hint("limit", "Daily limit reached",
			"The limit resets at 00:00 UTC.")}, nil
	}

	src := service.Source{Query: link, IsURL: true}
	results := make([]models.InlineQueryResult, 0, len(inlineFormats))
	for _, f := range inlineFormats {
		cached, err := h.svc.CachedFile(ctx, src, f.format)
		if err != nil {
			return nil, err
		}
		if cached != nil {
			results = append(results, cachedResult(f.format, f.title, cached))
			continue
		}
		results = append(results, &models.InlineQueryResultArticle{
			ID:          inlineDownload + ":" + string(f.format),
			Title:       f.title,
			Description: link,
			InputMessageContent: &models.InputTextMessageContent{
				MessageText:        fmt.Sprintf("⏳ Queued [%s]: %s", service.FormatLabel(string(f.format), defaultQuality(f.format)), link),
				LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tgbot.True()},
			},
			// The keyboard makes Telegram report an inline_message_id we can edit later.
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "⏳ Downloading…", CallbackData: "wait"},
			}}},
		})
	}
	return results, nil
}

func cachedResult(f ytdl.Format, title string, c *sqlc.FileCache) models.InlineQueryResult {
	id := inlineCached + ":" + string(f)
	caption := shorten(c.Title, 1024)
	switch service.MediaKind(c.MediaKind) {
	case service.MediaAudio:
		return &models.InlineQueryResultCachedAudio{ID: id, AudioFileID: c.TgFileID, Caption: caption}
	case service.MediaVideo:
		return &models.InlineQueryResultCachedVideo{ID: id, VideoFileID: c.TgFileID, Title: title, Description: c.Title, Caption: caption}
	}
	return &models.InlineQueryResultCachedDocument{ID: id, DocumentFileID: c.TgFileID, Title: title, Description: c.Title, Caption: caption}
}

func defaultQuality(f ytdl.Format) string {
	if f == ytdl.FormatMP4 {
		return string(ytdl.DefaultQuality)
	}
	return ""
}

// hint is a result that explains why there's nothing to download.
func hint(id, title, text string) models.InlineQueryResult {
	return &models.InlineQueryResultArticle{
		ID:                  "hint:" + id,
		Title:               title,
		Description:         text,
		InputMessageContent: &models.InputTextMessageContent{MessageText: text},
	}
}

// onChosenInlineResult starts the download for a placeholder the user just sent.
// It needs inline feedback enabled in @BotFather (/setinlinefeedback).
func (h *Handler) onChosenInlineResult(ctx context.Context, b *tgbot.Bot, r *models.ChosenInlineResult, u sqlc.User) {
	kind, format, _ := strings.Cut(r.ResultID, ":")
	f, ok := ytdl.ParseFormat(format)
	if !ok {
		return // a hint
	}
	link, ok := service.ParseURL(r.Query)
	if !ok {
		return
	}
	src := service.Source{Query: link, IsURL: true}

	if kind == inlineCached {
		if err := h.svc.RecordCachedInline(ctx, u.ID, src, f); err != nil {
			h.log.Warn("record cached inline result", "user_id", u.ID, "err", err)
		}
		return
	}
	if kind != inlineDownload || r.InlineMessageID == "" {
		return
	}

	_, err := h.svc.Submit(ctx, u.ID, service.KindInline, src, f, service.Target{InlineMessageID: r.InlineMessageID})
	if err == nil {
		return
	}
	text := h.submitErrorText(err)
	if text == "" {
		h.log.Error("submit inline request", "user_id", u.ID, "err", err)
		text = "❌ Couldn't start the download. Please try again later."
	}
	if _, err := b.EditMessageText(ctx, &tgbot.EditMessageTextParams{InlineMessageID: r.InlineMessageID, Text: text}); err != nil {
		h.log.Warn("edit inline message", "err", err)
	}
}
