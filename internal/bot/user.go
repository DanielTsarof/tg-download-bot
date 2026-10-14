package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/sqlc"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/service"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

const pendingText = "⏳ You don't have access yet. Your request has been sent to the admins; you'll get a message once it's approved."

const historyPageSize = 10

func (h *Handler) cmdStart(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, _ string) {
	if u.Status != service.StatusActive {
		h.reply(ctx, b, msg, "👋 Hi! This bot downloads audio and video by link or by name.\n\n"+pendingText)
		return
	}
	h.cmdHelp(ctx, b, msg, u, "")
}

func (h *Handler) cmdHelp(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, _ string) {
	var sb strings.Builder
	sb.WriteString("🎬 Send a link or a name and I'll return the file.\n\n")
	sb.WriteString("/mp3 <link or name> — audio (MP3)\n")
	sb.WriteString("/mp4 [360|480|720|1080|best] <link or name> — video (MP4), 480p by default\n")
	sb.WriteString("/wav <link or name> — audio (WAV)\n")
	sb.WriteString("Or just send a link or a name and pick the format.\n\n")
	if h.svc.InlineEnabled() {
		fmt.Fprintf(&sb, "In any chat: @%s <link> — pick a format and the file is posted right there.\n\n", h.username)
	}
	sb.WriteString("/history — your recent requests\n/me — your account and daily limit\n")
	if u.Role == service.RoleAdmin {
		sb.WriteString("\n👮 Admin:\n")
		for _, c := range adminCommands {
			fmt.Fprintf(&sb, "/%s — %s\n", c.Command, c.Description)
		}
	}
	h.reply(ctx, b, msg, sb.String())
}

func (h *Handler) cmdMe(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, _ string) {
	used, err := h.svc.UsedToday(ctx, u.ID)
	if err != nil {
		h.internalError(ctx, b, msg, "count requests", err)
		return
	}
	text := fmt.Sprintf("🆔 %d\n👤 %s\nRole: %s\nStatus: %s\nToday: %s",
		u.ID, displayName(u), u.Role, u.Status, h.usage(u, used))
	h.reply(ctx, b, msg, text)
}

func (h *Handler) cmdDownload(f ytdl.Format) func(context.Context, *tgbot.Bot, *models.Message, sqlc.User, string) {
	return func(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, args string) {
		var quality ytdl.Quality
		if f == ytdl.FormatMP4 {
			quality, args = splitQuality(args)
		}
		src, ok := service.ParseSource(args)
		if !ok {
			usage := fmt.Sprintf("Usage: /%s <link or name>\nExample: /%s https://www.youtube.com/watch?v=jNQXAC9IVRw", f, f)
			if f == ytdl.FormatMP4 {
				usage = mp4Usage
			}
			h.reply(ctx, b, msg, usage)
			return
		}
		src.Quality = quality
		h.startDownload(ctx, b, msg.Chat.ID, msg.ID, u, src, f)
	}
}

const mp4Usage = "Usage: /mp4 [360|480|720|1080|best] <link or name>\n" +
	"The resolution is optional (480p by default).\n" +
	"Example: /mp4 720 https://www.youtube.com/watch?v=jNQXAC9IVRw"

// splitQuality takes a leading resolution off /mp4 arguments. A resolution
// alone is kept as the search text, so a name like "1080" still works.
func splitQuality(args string) (ytdl.Quality, string) {
	first, rest, found := strings.Cut(strings.TrimSpace(args), " ")
	if !found || strings.TrimSpace(rest) == "" {
		return "", args
	}
	q, ok := ytdl.ParseQuality(first)
	if !ok {
		return "", args
	}
	return q, strings.TrimSpace(rest)
}

func (h *Handler) startDownload(ctx context.Context, b *tgbot.Bot, chatID int64, replyTo int, u sqlc.User, src service.Source, f ytdl.Format) {
	_, err := h.svc.Submit(ctx, u.ID, service.KindCommand, src, f, service.Target{ChatID: chatID, ReplyTo: replyTo})
	if err == nil {
		return
	}
	text := h.submitErrorText(err)
	if text == "" {
		h.log.Error("submit request", "user_id", u.ID, "err", err)
		text = "❌ Couldn't start the download. Please try again later."
	}
	h.send(ctx, b, &tgbot.SendMessageParams{
		ChatID: chatID, Text: text,
		ReplyParameters: &models.ReplyParameters{MessageID: replyTo, AllowSendingWithoutReply: true},
	})
}

// submitErrorText explains expected Submit errors; "" means unexpected.
func (h *Handler) submitErrorText(err error) string {
	var limitErr *service.LimitError
	switch {
	case errors.As(err, &limitErr):
		return fmt.Sprintf("🚫 You've used all %d requests for today. The limit resets at 00:00 UTC.", limitErr.Limit)
	case errors.Is(err, service.ErrNoAccess):
		return pendingText
	}
	return ""
}

func (h *Handler) cmdHistory(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, args string) {
	page := parsePage(args)
	reqs, err := h.q.ListUserRequests(ctx, sqlc.ListUserRequestsParams{
		UserID: u.ID, Limit: historyPageSize, Offset: int32((page - 1) * historyPageSize),
	})
	if err != nil {
		h.internalError(ctx, b, msg, "list history", err)
		return
	}
	total, err := h.q.CountUserRequests(ctx, u.ID)
	if err != nil {
		h.internalError(ctx, b, msg, "count history", err)
		return
	}
	if len(reqs) == 0 {
		h.reply(ctx, b, msg, "No requests yet.")
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🕘 Your requests (page %d/%d):\n\n", page, pages(total, historyPageSize))
	for _, r := range reqs {
		sb.WriteString(requestLine(r, false))
		sb.WriteString("\n")
	}
	if int64(page*historyPageSize) < total {
		fmt.Fprintf(&sb, "\nNext: /history %d", page+1)
	}
	h.reply(ctx, b, msg, sb.String())
}

// onPlainText offers a format for a link or name sent without a command.
func (h *Handler) onPlainText(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, text string) {
	if u.Status != service.StatusActive {
		h.reply(ctx, b, msg, pendingText)
		return
	}
	if _, ok := service.ParseSource(text); !ok {
		h.reply(ctx, b, msg, "That's too long for a search name. Send a link or a shorter name.")
		return
	}
	// The picker replies to the user's message, so the callback can read the query from it.
	h.send(ctx, b, &tgbot.SendMessageParams{
		ChatID:          msg.Chat.ID,
		Text:            "Which format?",
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "🎵 MP3", CallbackData: "fmt:mp3"},
			{Text: "🎬 MP4", CallbackData: "fmt:mp4"},
			{Text: "🎼 WAV", CallbackData: "fmt:wav"},
		}}},
	})
}

func (h *Handler) onCallback(ctx context.Context, b *tgbot.Bot, cq *models.CallbackQuery, u sqlc.User) {
	action, arg, _ := strings.Cut(cq.Data, ":")
	answer := ""
	switch action {
	case "fmt":
		answer = h.onFormatChosen(ctx, b, cq, u, arg)
	case "grant", "deny":
		answer = h.onAccessDecision(ctx, b, cq, u, action, arg)
	case "wait":
		answer = "Downloading, please wait…"
	}
	if _, err := b.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID, Text: answer,
	}); err != nil {
		h.log.Debug("answer callback", "err", err)
	}
}

func (h *Handler) onFormatChosen(ctx context.Context, b *tgbot.Bot, cq *models.CallbackQuery, u sqlc.User, arg string) string {
	f, ok := ytdl.ParseFormat(arg)
	picker := cq.Message.Message
	if !ok || picker == nil {
		return "This button has expired."
	}
	if u.Status != service.StatusActive {
		return "You don't have access yet."
	}
	orig := picker.ReplyToMessage
	if orig == nil || orig.From == nil || orig.From.ID != u.ID {
		return "Send the link or name again."
	}
	src, ok := service.ParseSource(orig.Text)
	if !ok {
		return "Send the link or name again."
	}
	if _, err := b.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: picker.Chat.ID, MessageID: picker.ID}); err != nil {
		h.log.Debug("delete format picker", "err", err)
	}
	h.startDownload(ctx, b, picker.Chat.ID, orig.ID, u, src, f)
	return ""
}

func (h *Handler) notifyAdminsAboutNewUser(ctx context.Context, b *tgbot.Bot, u sqlc.User) {
	admins, err := h.q.ListAdmins(ctx)
	if err != nil {
		h.log.Error("list admins", "err", err)
		return
	}
	id := strconv.FormatInt(u.ID, 10)
	for _, a := range admins {
		h.send(ctx, b, &tgbot.SendMessageParams{
			ChatID: a.ID,
			Text:   fmt.Sprintf("🆕 Access request from %s (id %d).", displayName(u), u.ID),
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "✅ Grant", CallbackData: "grant:" + id},
				{Text: "🚫 Deny", CallbackData: "deny:" + id},
			}}},
		})
	}
}

func (h *Handler) onAccessDecision(ctx context.Context, b *tgbot.Bot, cq *models.CallbackQuery, admin sqlc.User, action, arg string) string {
	if admin.Role != service.RoleAdmin || admin.Status != service.StatusActive {
		return "Admins only."
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return "Bad button."
	}
	var (
		u   sqlc.User
		res string
	)
	if action == "grant" {
		u, err = h.q.GrantUser(ctx, sqlc.GrantUserParams{ID: id})
		res = "✅ Granted"
	} else {
		if id == admin.ID {
			return "You can't block yourself."
		}
		u, err = h.q.SetUserStatus(ctx, sqlc.SetUserStatusParams{ID: id, Status: service.StatusBlocked})
		res = "🚫 Denied"
	}
	if err != nil {
		h.log.Error("access decision", "user_id", id, "err", err)
		return "Failed, see logs."
	}
	if m := cq.Message.Message; m != nil {
		_, _ = b.EditMessageText(ctx, &tgbot.EditMessageTextParams{
			ChatID: m.Chat.ID, MessageID: m.ID,
			Text: fmt.Sprintf("%s\n%s by %s", m.Text, res, displayName(admin)),
		})
	}
	h.notifyUserStatus(ctx, b, u)
	return res
}

// notifyUserStatus tells a user their access changed.
func (h *Handler) notifyUserStatus(ctx context.Context, b *tgbot.Bot, u sqlc.User) {
	var text string
	switch u.Status {
	case service.StatusActive:
		text = fmt.Sprintf("✅ You now have access. Daily limit: %s. Send /help to get started.", h.limitText(u))
	case service.StatusBlocked:
		text = "🚫 Your access to this bot has been revoked."
	default:
		return
	}
	h.send(ctx, b, &tgbot.SendMessageParams{ChatID: u.ID, Text: text})
}

func (h *Handler) internalError(ctx context.Context, b *tgbot.Bot, msg *models.Message, what string, err error) {
	h.log.Error(what, "err", err)
	h.reply(ctx, b, msg, "❌ Something went wrong. Please try again later.")
}
