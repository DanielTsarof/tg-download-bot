// Package bot holds the Telegram update handlers.
package bot

import (
	"context"
	"log/slog"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/sqlc"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/service"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

type Handler struct {
	svc      *service.Service
	q        *sqlc.Queries
	log      *slog.Logger
	username string // the bot's @username, for /cmd@bot and inline hints

	commands map[string]command
}

type access int

const (
	accessAnyone access = iota // even pending users
	accessActive
	accessAdmin
)

type command struct {
	access  access
	handler func(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User, args string)
}

func New(svc *service.Service, botUsername string, log *slog.Logger) *Handler {
	h := &Handler{svc: svc, q: svc.Queries(), log: log, username: botUsername}
	h.commands = map[string]command{
		"start":   {accessAnyone, h.cmdStart},
		"help":    {accessAnyone, h.cmdHelp},
		"me":      {accessAnyone, h.cmdMe},
		"mp3":     {accessActive, h.cmdDownload(ytdl.FormatMP3)},
		"mp4":     {accessActive, h.cmdDownload(ytdl.FormatMP4)},
		"wav":     {accessActive, h.cmdDownload(ytdl.FormatWAV)},
		"history": {accessActive, h.cmdHistory},

		"users":    {accessAdmin, h.cmdUsers},
		"user":     {accessAdmin, h.cmdUser},
		"requests": {accessAdmin, h.cmdRequests},
		"grant":    {accessAdmin, h.cmdGrant},
		"revoke":   {accessAdmin, h.cmdRevoke},
		"limit":    {accessAdmin, h.cmdLimit},
		"promote":  {accessAdmin, h.cmdPromote},
		"demote":   {accessAdmin, h.cmdDemote},
	}
	return h
}

type userKey struct{}

func userFrom(ctx context.Context) (sqlc.User, bool) {
	u, ok := ctx.Value(userKey{}).(sqlc.User)
	return u, ok
}

// UserMiddleware records the sender of every update and puts them in the context.
func (h *Handler) UserMiddleware(next tgbot.HandlerFunc) tgbot.HandlerFunc {
	return func(ctx context.Context, b *tgbot.Bot, upd *models.Update) {
		from := sender(upd)
		if from == nil || from.IsBot {
			return
		}
		row, err := h.q.UpsertUser(ctx, sqlc.UpsertUserParams{
			ID: from.ID, Username: from.Username, FirstName: from.FirstName,
		})
		if err != nil {
			h.log.Error("upsert user", "user_id", from.ID, "err", err)
			return
		}
		u := sqlc.User{
			ID: row.ID, Username: row.Username, FirstName: row.FirstName, Role: row.Role,
			Status: row.Status, DailyLimit: row.DailyLimit, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}
		if u.Status == service.StatusBlocked {
			return
		}
		if row.Inserted && u.Status == service.StatusPending {
			h.notifyAdminsAboutNewUser(ctx, b, u)
		}
		next(context.WithValue(ctx, userKey{}, u), b, upd)
	}
}

func sender(upd *models.Update) *models.User {
	switch {
	case upd.Message != nil:
		return upd.Message.From
	case upd.CallbackQuery != nil:
		return &upd.CallbackQuery.From
	case upd.InlineQuery != nil:
		return upd.InlineQuery.From
	case upd.ChosenInlineResult != nil:
		return &upd.ChosenInlineResult.From
	}
	return nil
}

// Handle dispatches an update. Register it as the bot's default handler.
func (h *Handler) Handle(ctx context.Context, b *tgbot.Bot, upd *models.Update) {
	u, ok := userFrom(ctx)
	if !ok {
		return
	}
	switch {
	case upd.Message != nil:
		h.onMessage(ctx, b, upd.Message, u)
	case upd.CallbackQuery != nil:
		h.onCallback(ctx, b, upd.CallbackQuery, u)
	case upd.InlineQuery != nil:
		h.onInlineQuery(ctx, b, upd.InlineQuery, u)
	case upd.ChosenInlineResult != nil:
		h.onChosenInlineResult(ctx, b, upd.ChosenInlineResult, u)
	}
}

func (h *Handler) onMessage(ctx context.Context, b *tgbot.Bot, msg *models.Message, u sqlc.User) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	name, args, isCmd := parseCommand(text)
	if !isCmd {
		// In groups the bot only reacts to commands.
		if msg.Chat.Type == models.ChatTypePrivate {
			h.onPlainText(ctx, b, msg, u, text)
		}
		return
	}
	name, ok := h.forThisBot(name)
	if !ok {
		return
	}
	cmd, ok := h.commands[name]
	if !ok {
		h.reply(ctx, b, msg, "Unknown command. See /help.")
		return
	}
	switch {
	case cmd.access >= accessActive && u.Status != service.StatusActive:
		h.reply(ctx, b, msg, pendingText)
		return
	case cmd.access == accessAdmin && u.Role != service.RoleAdmin:
		h.reply(ctx, b, msg, "This command is for admins only.")
		return
	}
	cmd.handler(ctx, b, msg, u, args)
}

// parseCommand splits "/name@bot args" into "name@bot" and "args".
func parseCommand(text string) (name, args string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name, args, _ = strings.Cut(text[1:], " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		args = name[i+1:] + " " + args
		name = name[:i]
	}
	return strings.ToLower(name), strings.TrimSpace(args), name != ""
}

// forThisBot strips "@botname" and reports whether the command is addressed to this bot.
func (h *Handler) forThisBot(name string) (string, bool) {
	cmd, target, found := strings.Cut(name, "@")
	if found && !strings.EqualFold(target, h.username) {
		return "", false
	}
	return cmd, true
}

func (h *Handler) reply(ctx context.Context, b *tgbot.Bot, msg *models.Message, text string) {
	h.send(ctx, b, &tgbot.SendMessageParams{
		ChatID:             msg.Chat.ID,
		Text:               text,
		ReplyParameters:    &models.ReplyParameters{MessageID: msg.ID, AllowSendingWithoutReply: true},
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tgbot.True()},
	})
}

func (h *Handler) send(ctx context.Context, b *tgbot.Bot, params *tgbot.SendMessageParams) {
	if params.LinkPreviewOptions == nil {
		params.LinkPreviewOptions = &models.LinkPreviewOptions{IsDisabled: tgbot.True()}
	}
	if _, err := b.SendMessage(ctx, params); err != nil {
		h.log.Warn("send message", "chat_id", params.ChatID, "err", err)
	}
}

// SetCommands publishes the command menus: one for everyone, a longer one per admin.
func (h *Handler) SetCommands(ctx context.Context, b *tgbot.Bot) {
	if _, err := b.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{Commands: userCommands}); err != nil {
		h.log.Warn("set commands", "err", err)
	}
	admins, err := h.q.ListAdmins(ctx)
	if err != nil {
		h.log.Warn("list admins", "err", err)
		return
	}
	for _, a := range admins {
		h.setAdminCommands(ctx, b, a.ID, true)
	}
}

func (h *Handler) setAdminCommands(ctx context.Context, b *tgbot.Bot, userID int64, admin bool) {
	scope := &models.BotCommandScopeChat{ChatID: userID}
	var err error
	if admin {
		_, err = b.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{
			Commands: append(append([]models.BotCommand{}, userCommands...), adminCommands...),
			Scope:    scope,
		})
	} else {
		_, err = b.DeleteMyCommands(ctx, &tgbot.DeleteMyCommandsParams{Scope: scope})
	}
	if err != nil {
		// Fails until the admin has started a chat with the bot.
		h.log.Debug("set admin commands", "user_id", userID, "err", err)
	}
}

var userCommands = []models.BotCommand{
	{Command: "mp3", Description: "Audio (MP3) by link or name"},
	{Command: "mp4", Description: "Video: /mp4 [360|480|720|1080|best] <link or name>"},
	{Command: "wav", Description: "Audio (WAV) by link or name"},
	{Command: "history", Description: "Your recent requests"},
	{Command: "me", Description: "Your account and limit"},
	{Command: "help", Description: "How to use the bot"},
}

var adminCommands = []models.BotCommand{
	{Command: "users", Description: "List users"},
	{Command: "user", Description: "User details: /user <id|@username>"},
	{Command: "requests", Description: "Recent requests: /requests [user] [page]"},
	{Command: "grant", Description: "Grant access: /grant <user> [limit]"},
	{Command: "revoke", Description: "Block a user: /revoke <user>"},
	{Command: "limit", Description: "Daily limit: /limit <user> <n|default>"},
	{Command: "promote", Description: "Make admin: /promote <user>"},
	{Command: "demote", Description: "Remove admin: /demote <user>"},
}
