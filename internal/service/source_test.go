package service

import (
	"strings"
	"testing"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		in    string
		want  Source
		valid bool
	}{
		{"https://www.youtube.com/watch?v=jNQXAC9IVRw", Source{Query: "https://www.youtube.com/watch?v=jNQXAC9IVRw", IsURL: true}, true},
		{"  me   at the\nzoo ", Source{Query: "me at the zoo", IsURL: false}, true},
		{"ftp://example.com/a", Source{Query: "ftp://example.com/a", IsURL: false}, true}, // not a supported link: searched as a name
		{"youtube.com/watch?v=1", Source{Query: "youtube.com/watch?v=1", IsURL: false}, true},
		{"", Source{}, false},
		{"   ", Source{}, false},
		{strings.Repeat("a", 301), Source{}, false},
	}
	for _, tt := range tests {
		got, ok := ParseSource(tt.in)
		if ok != tt.valid || got != tt.want {
			t.Errorf("ParseSource(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, tt.want, tt.valid)
		}
	}
}

func TestCacheKey(t *testing.T) {
	a := Source{Query: "HTTPS://WWW.YouTube.com/watch?v=jNQXAC9IVRw#t=10", IsURL: true}.cacheKey()
	b := Source{Query: "https://www.youtube.com/watch?v=jNQXAC9IVRw", IsURL: true}.cacheKey()
	if a != b {
		t.Errorf("cache keys differ: %q vs %q", a, b)
	}
	// Paths and queries are case-sensitive.
	if c := (Source{Query: "https://www.youtube.com/watch?v=JNQXAC9IVRW", IsURL: true}).cacheKey(); c == b {
		t.Errorf("video IDs must not be case-folded: %q", c)
	}
	if n, m := (Source{Query: "Me At The Zoo"}).cacheKey(), (Source{Query: "me at the zoo"}).cacheKey(); n != m {
		t.Errorf("name keys differ: %q vs %q", n, m)
	}
}

func TestSafeFilename(t *testing.T) {
	if got := safeFilename(`AC/DC: "Back" <in> Black?`); got != "AC_DC_ _Back_ _in_ Black_" {
		t.Errorf("safeFilename = %q", got)
	}
	if got := safeFilename("  "); got != "file" {
		t.Errorf("safeFilename(blank) = %q", got)
	}
}

func TestWithFormat(t *testing.T) {
	src := Source{Query: "x"}
	if got := src.withFormat(ytdl.FormatMP4).Quality; got != ytdl.Quality480 {
		t.Errorf("default video quality = %q", got)
	}
	if got := (Source{Query: "x", Quality: ytdl.Quality720}).withFormat(ytdl.FormatMP4).Quality; got != ytdl.Quality720 {
		t.Errorf("explicit quality = %q", got)
	}
	if got := (Source{Query: "x", Quality: ytdl.Quality720}).withFormat(ytdl.FormatMP3).Quality; got != "" {
		t.Errorf("audio quality = %q", got)
	}
}

func TestFormatLabel(t *testing.T) {
	for _, tt := range [][3]string{{"mp4", "720", "MP4 720p"}, {"mp4", "best", "MP4 best"}, {"mp3", "", "MP3"}} {
		if got := FormatLabel(tt[0], tt[1]); got != tt[2] {
			t.Errorf("FormatLabel(%q, %q) = %q", tt[0], tt[1], got)
		}
	}
}
