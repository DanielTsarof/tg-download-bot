package service

import (
	"net/url"
	"strings"

	"tg-file-downloader/internal/ytdl"
)

// Source is what the user asked for: a link or a search name.
type Source struct {
	Query string
	IsURL bool
}

// ParseSource classifies user input as a link (absolute http/https URL) or a search name.
func ParseSource(input string) (Source, bool) {
	q := strings.Join(strings.Fields(input), " ")
	if q == "" {
		return Source{}, false
	}
	if u, ok := ParseURL(q); ok {
		return Source{Query: u, IsURL: true}, true
	}
	if len(q) > 300 {
		return Source{}, false
	}
	return Source{Query: q}, true
}

// ParseURL returns s if it is a single absolute http(s) URL the service accepts.
func ParseURL(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\n") || len(s) > 2048 {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return s, true
}

// cacheKey identifies the source in file_cache.
func (s Source) cacheKey() string {
	if !s.IsURL {
		return "name:" + strings.ToLower(s.Query)
	}
	u, err := url.Parse(s.Query)
	if err != nil {
		return "url:" + s.Query
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	return "url:" + u.String()
}

func (s Source) taskRequest(f ytdl.Format) ytdl.CreateTaskRequest {
	if s.IsURL {
		return ytdl.CreateTaskRequest{Format: f, URL: s.Query}
	}
	return ytdl.CreateTaskRequest{Format: f, Name: s.Query}
}
