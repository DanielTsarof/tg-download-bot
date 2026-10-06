package bot

import (
	"testing"

	"tg-file-downloader/internal/ytdl"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in, name, args string
		ok             bool
	}{
		{"/mp3 https://youtu.be/x", "mp3", "https://youtu.be/x", true},
		{"/MP4@DlBot  me at the zoo ", "mp4@dlbot", "me at the zoo", true},
		{"/grant\n123 5", "grant", "123 5", true},
		{"/history", "history", "", true},
		{"hello", "", "", false},
		{"/", "", "", false},
	}
	for _, tt := range tests {
		name, args, ok := parseCommand(tt.in)
		if name != tt.name || args != tt.args || ok != tt.ok {
			t.Errorf("parseCommand(%q) = %q, %q, %v", tt.in, name, args, ok)
		}
	}
}

func TestForThisBot(t *testing.T) {
	h := &Handler{username: "DlBot"}
	if name, ok := h.forThisBot("mp3@dlbot"); !ok || name != "mp3" {
		t.Errorf("own command rejected: %q %v", name, ok)
	}
	if _, ok := h.forThisBot("mp3@otherbot"); ok {
		t.Error("command for another bot accepted")
	}
	if name, ok := h.forThisBot("mp3"); !ok || name != "mp3" {
		t.Errorf("plain command rejected: %q %v", name, ok)
	}
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]bool{"0": true, "25": true, "-1": false, "x": false, "99999999999": false} {
		if _, ok := parseLimit(in); ok != want {
			t.Errorf("parseLimit(%q) ok = %v", in, ok)
		}
	}
}

func TestSplitQuality(t *testing.T) {
	tests := []struct {
		in      string
		quality ytdl.Quality
		rest    string
	}{
		{"720 https://youtu.be/x", ytdl.Quality720, "https://youtu.be/x"},
		{"best me at the zoo", ytdl.QualityBest, "me at the zoo"},
		{"1080p hits", ytdl.Quality1080, "hits"},
		{"https://youtu.be/x", "", "https://youtu.be/x"},
		{"me at the zoo", "", "me at the zoo"},
		{"720", "", "720"}, // a bare number is a search
		{"4k movie", "", "4k movie"},
	}
	for _, tt := range tests {
		q, rest := splitQuality(tt.in)
		if q != tt.quality || rest != tt.rest {
			t.Errorf("splitQuality(%q) = %q, %q", tt.in, q, rest)
		}
	}
}
