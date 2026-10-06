package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/jackc/pgx/v5"

	"tg-file-downloader/internal/db/sqlc"
	"tg-file-downloader/internal/service"
)

const (
	usersPageSize    = 20
	requestsPageSize = 15
)

var errUnknownUser = errors.New("unknown user")

var userStatusIcons = map[string]string{
	service.StatusActive:  "🟢",
	service.StatusPending: "🟡",
	service.StatusBlocked: "🔴",
}

// resolveUser finds a user by numeric Telegram ID or @username.
func (h *Handler) resolveUser(ctx context.Context, ref string) (sqlc.User, error) {
	var (
		u   sqlc.User
		err error
	)
	if name, ok := strings.CutPrefix(ref, "@"); ok {
		u, err = h.q.GetUserByUsername(ctx, name)
	} else if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
		u, err = h.q.GetUser(ctx, id)
	} else {
		return u, errUnknownUser
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return u, errUnknownUser
	}
	return u, err
}

// userArg resolves the user in args[0], replying with the problem if it fails.
func (h *Handler) userArg(ctx context.Context, b *tgbot.Bot, msg *models.Message, args []string, usage string) (sqlc.User, bool) {
	if len(args) == 0 {
		h.reply(ctx, b, msg, "Usage: "+usage)
		return sqlc.User{}, false
	}
	u, err := h.resolveUser(ctx, args[0])
	if errors.Is(err, errUnknownUser) {
		h.reply(ctx, b, msg, fmt.Sprintf("User %s not found. Users appear once they write to the bot; /grant also accepts a numeric ID of someone who hasn't yet.", args[0]))
		return u, false
	}
	if err != nil {
		h.internalError(ctx, b, msg, "resolve user", err)
		return u, false
	}
	return u, true
}

func (h *Handler) cmdUsers(ctx context.Context, b *tgbot.Bot, msg *models.Message, _ sqlc.User, args string) {
	page := parsePage(args)
	rows, err := h.q.ListUsersWithUsage(ctx, sqlc.ListUsersWithUsageParams{
		Limit: usersPageSize, Offset: int32((page - 1) * usersPageSize), Since: service.StartOfDay(time.Now()),
	})
	if err != nil {
		h.internalError(ctx, b, msg, "list users", err)
		return
	}
	total, err := h.q.CountUsers(ctx)
	if err != nil {
		h.internalError(ctx, b, msg, "count users", err)
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 Users: %d (page %d/%d)\n🟢 active  🟡 pending  🔴 blocked\n\n", total, page, pages(total, usersPageSize))
	for _, r := range rows {
		u := sqlc.User{
			ID: r.ID, Username: r.Username, FirstName: r.FirstName, Role: r.Role,
			Status: r.Status, DailyLimit: r.DailyLimit,
		}
		role := ""
		if u.Role == service.RoleAdmin {
			role = " 👮"
		}
		fmt.Fprintf(&sb, "%s %d %s%s — today %s\n", userStatusIcons[u.Status], u.ID, displayName(u), role, h.usage(u, r.UsedToday))
	}
	if int64(page*usersPageSize) < total {
		fmt.Fprintf(&sb, "\nNext: /users %d", page+1)
	}
	sb.WriteString("\nDetails: /user <id>")
	h.reply(ctx, b, msg, sb.String())
}

func (h *Handler) cmdUser(ctx context.Context, b *tgbot.Bot, msg *models.Message, _ sqlc.User, args string) {
	u, ok := h.userArg(ctx, b, msg, strings.Fields(args), "/user <id|@username>")
	if !ok {
		return
	}
	stats, err := h.q.UserRequestStats(ctx, sqlc.UserRequestStatsParams{UserID: u.ID, Since: service.StartOfDay(time.Now())})
	if err != nil {
		h.internalError(ctx, b, msg, "user stats", err)
		return
	}
	recent, err := h.q.ListUserRequests(ctx, sqlc.ListUserRequestsParams{UserID: u.ID, Limit: 5})
	if err != nil {
		h.internalError(ctx, b, msg, "user requests", err)
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n🆔 %d\nRole: %s\nStatus: %s\nDaily limit: %s\nToday: %s\nRegistered: %s\n\n",
		userStatusIcons[u.Status], displayName(u), u.ID, u.Role, u.Status, h.limitText(u),
		h.usage(u, stats.Today), formatTime(u.CreatedAt))
	fmt.Fprintf(&sb, "📊 Requests: %d total, %d ok, %d failed, %d in progress\nLast request: %s\n",
		stats.Total, stats.Succeeded, stats.Failed, stats.InProgress, formatTime(stats.LastRequestAt))
	if len(recent) > 0 {
		sb.WriteString("\nRecent:\n")
		for _, r := range recent {
			sb.WriteString(requestLine(r, false))
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "\nAll: /requests %d", u.ID)
	}
	h.reply(ctx, b, msg, sb.String())
}

func (h *Handler) cmdRequests(ctx context.Context, b *tgbot.Bot, msg *models.Message, _ sqlc.User, args string) {
	fields := strings.Fields(args)
	page := 1
	var user *sqlc.User
	switch len(fields) {
	case 0:
	case 1:
		// A lone argument is a user if it names one, otherwise a page number.
		if u, err := h.resolveUser(ctx, fields[0]); err == nil {
			user = &u
		} else if strings.HasPrefix(fields[0], "@") {
			h.reply(ctx, b, msg, "User "+fields[0]+" not found.")
			return
		} else {
			page = parsePage(fields[0])
		}
	default:
		u, ok := h.userArg(ctx, b, msg, fields, "/requests [user] [page]")
		if !ok {
			return
		}
		user, page = &u, parsePage(fields[1])
	}

	offset := int32((page - 1) * requestsPageSize)
	var (
		reqs  []sqlc.Request
		total int64
		err   error
	)
	if user != nil {
		reqs, err = h.q.ListUserRequests(ctx, sqlc.ListUserRequestsParams{UserID: user.ID, Limit: requestsPageSize, Offset: offset})
		if err == nil {
			total, err = h.q.CountUserRequests(ctx, user.ID)
		}
	} else {
		reqs, err = h.q.ListRecentRequests(ctx, sqlc.ListRecentRequestsParams{Limit: requestsPageSize, Offset: offset})
		if err == nil {
			total, err = h.q.CountRequests(ctx)
		}
	}
	if err != nil {
		h.internalError(ctx, b, msg, "list requests", err)
		return
	}
	if len(reqs) == 0 {
		h.reply(ctx, b, msg, "No requests.")
		return
	}

	var sb strings.Builder
	next := fmt.Sprintf("/requests %d", page+1)
	if user != nil {
		fmt.Fprintf(&sb, "📜 Requests of %s", displayName(*user))
		next = fmt.Sprintf("/requests %d %d", user.ID, page+1)
	} else {
		sb.WriteString("📜 All requests")
	}
	fmt.Fprintf(&sb, " — %d (page %d/%d), times UTC:\n\n", total, page, pages(total, requestsPageSize))
	for _, r := range reqs {
		sb.WriteString(requestLine(r, user == nil))
		sb.WriteString("\n")
	}
	if int64(page*requestsPageSize) < total {
		sb.WriteString("\nNext: " + next)
	}
	h.reply(ctx, b, msg, sb.String())
}

func (h *Handler) cmdGrant(ctx context.Context, b *tgbot.Bot, msg *models.Message, _ sqlc.User, args string) {
	const usage = "/grant <id|@username> [daily limit]"
	fields := strings.Fields(args)
	if len(fields) == 0 || len(fields) > 2 {
		h.reply(ctx, b, msg, "Usage: "+usage)
		return
	}
	var id int64
	if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil && n > 0 {
		id = n // may not have written to the bot yet
	} else {
		u, ok := h.userArg(ctx, b, msg, fields, usage)
		if !ok {
			return
		}
		id = u.ID
	}
	var limit *int32
	if len(fields) == 2 {
		n, ok := parseLimit(fields[1])
		if !ok {
			h.reply(ctx, b, msg, "The limit must be a non-negative number.")
			return
		}
		limit = &n
	}
	u, err := h.q.GrantUser(ctx, sqlc.GrantUserParams{ID: id, DailyLimit: limit})
	if err != nil {
		h.internalError(ctx, b, msg, "grant user", err)
		return
	}
	h.reply(ctx, b, msg, fmt.Sprintf("✅ %s has access. Daily limit: %s.", displayName(u), h.limitText(u)))
	h.notifyUserStatus(ctx, b, u)
}

func (h *Handler) cmdRevoke(ctx context.Context, b *tgbot.Bot, msg *models.Message, admin sqlc.User, args string) {
	u, ok := h.userArg(ctx, b, msg, strings.Fields(args), "/revoke <id|@username>")
	if !ok {
		return
	}
	if u.ID == admin.ID {
		h.reply(ctx, b, msg, "You can't revoke your own access.")
		return
	}
	u, err := h.q.SetUserStatus(ctx, sqlc.SetUserStatusParams{ID: u.ID, Status: service.StatusBlocked})
	if err != nil {
		h.internalError(ctx, b, msg, "revoke user", err)
		return
	}
	h.reply(ctx, b, msg, fmt.Sprintf("🚫 %s is blocked. /grant %d restores access.", displayName(u), u.ID))
	h.notifyUserStatus(ctx, b, u)
}

func (h *Handler) cmdLimit(ctx context.Context, b *tgbot.Bot, msg *models.Message, _ sqlc.User, args string) {
	const usage = "/limit <id|@username> <n|default>"
	fields := strings.Fields(args)
	if len(fields) != 2 {
		h.reply(ctx, b, msg, "Usage: "+usage)
		return
	}
	u, ok := h.userArg(ctx, b, msg, fields, usage)
	if !ok {
		return
	}
	var limit *int32
	if !strings.EqualFold(fields[1], "default") {
		n, ok := parseLimit(fields[1])
		if !ok {
			h.reply(ctx, b, msg, "The limit must be a non-negative number or \"default\".")
			return
		}
		limit = &n
	}
	u, err := h.q.SetUserDailyLimit(ctx, sqlc.SetUserDailyLimitParams{ID: u.ID, DailyLimit: limit})
	if err != nil {
		h.internalError(ctx, b, msg, "set limit", err)
		return
	}
	text := fmt.Sprintf("📏 Daily limit of %s: %s.", displayName(u), h.limitText(u))
	if u.Role == service.RoleAdmin {
		text += " (Admins aren't limited while they are admins.)"
	}
	h.reply(ctx, b, msg, text)
}

func (h *Handler) cmdPromote(ctx context.Context, b *tgbot.Bot, msg *models.Message, admin sqlc.User, args string) {
	h.setRole(ctx, b, msg, admin, args, service.RoleAdmin)
}

func (h *Handler) cmdDemote(ctx context.Context, b *tgbot.Bot, msg *models.Message, admin sqlc.User, args string) {
	h.setRole(ctx, b, msg, admin, args, service.RoleUser)
}

func (h *Handler) setRole(ctx context.Context, b *tgbot.Bot, msg *models.Message, admin sqlc.User, args, role string) {
	u, ok := h.userArg(ctx, b, msg, strings.Fields(args), "/promote|/demote <id|@username>")
	if !ok {
		return
	}
	if u.ID == admin.ID {
		h.reply(ctx, b, msg, "You can't change your own role.")
		return
	}
	if role == service.RoleAdmin && u.Status != service.StatusActive {
		h.reply(ctx, b, msg, fmt.Sprintf("Grant access first: /grant %d", u.ID))
		return
	}
	u, err := h.q.SetUserRole(ctx, sqlc.SetUserRoleParams{ID: u.ID, Role: role})
	if err != nil {
		h.internalError(ctx, b, msg, "set role", err)
		return
	}
	h.setAdminCommands(ctx, b, u.ID, role == service.RoleAdmin)
	h.reply(ctx, b, msg, fmt.Sprintf("👮 %s is now %s.", displayName(u), role))
}

func parseLimit(s string) (int32, bool) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}
