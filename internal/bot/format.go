package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/sqlc"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/service"
)

func displayName(u sqlc.User) string {
	switch {
	case u.Username != "" && u.FirstName != "":
		return fmt.Sprintf("%s (@%s)", u.FirstName, u.Username)
	case u.Username != "":
		return "@" + u.Username
	case u.FirstName != "":
		return u.FirstName
	}
	return "user " + strconv.FormatInt(u.ID, 10)
}

func (h *Handler) limitText(u sqlc.User) string {
	limit, limited := h.svc.DailyLimit(u)
	switch {
	case !limited:
		return "unlimited"
	case u.DailyLimit == nil:
		return fmt.Sprintf("%d/day (default)", limit)
	}
	return fmt.Sprintf("%d/day", limit)
}

// usage renders "used/limit" for today.
func (h *Handler) usage(u sqlc.User, used int64) string {
	limit, limited := h.svc.DailyLimit(u)
	if !limited {
		return fmt.Sprintf("%d requests (unlimited)", used)
	}
	return fmt.Sprintf("%d/%d requests", used, limit)
}

var statusIcons = map[string]string{
	"queued":    "⏳",
	"running":   "⬇️",
	"succeeded": "✅",
	"failed":    "❌",
}

// requestLine renders one history entry; withUser adds the requester's ID.
func requestLine(r sqlc.Request, withUser bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s #%d %s %s", statusIcons[r.Status], r.ID, r.CreatedAt.UTC().Format("01-02 15:04"), service.FormatLabel(r.Format, r.Quality))
	if withUser {
		fmt.Fprintf(&sb, " user %d", r.UserID)
	}
	if r.Kind == "inline" {
		sb.WriteString(" inline")
	}
	if r.FromCache {
		sb.WriteString(" (cache)")
	}
	sb.WriteString("\n   ")
	sb.WriteString(shorten(r.Query, 80))
	if r.Title != "" && r.Title != r.Query {
		sb.WriteString("\n   «" + shorten(r.Title, 60) + "»")
	}
	if r.Error != "" {
		sb.WriteString("\n   ⚠️ " + shorten(r.Error, 120))
	}
	return sb.String()
}

func formatTime(t time.Time) string {
	if t.Unix() <= 0 {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func parsePage(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func pages(total int64, size int) int64 {
	return max((total+int64(size)-1)/int64(size), 1)
}
