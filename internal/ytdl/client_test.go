package ytdl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ytdl_key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "idem-1" {
			t.Errorf("Idempotency-Key = %q", got)
		}
		var body CreateTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Format != FormatMP3 || body.Name != "me at the zoo" || body.URL != "" {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"7b2c5a3e-0000-4000-8000-000000000001","status":"queued","format":"mp3",
			"source_id":null,"error":null,"webhook_url":null,"webhook_state":"none","webhook_error":null,
			"created_at":"2026-10-06T10:00:00Z","completed_at":null,"expires_at":null,
			"status_url":"/v1/tasks/7b2c5a3e-0000-4000-8000-000000000001","file_available":false}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "ytdl_key", nil)
	task, err := c.CreateTask(context.Background(), CreateTaskRequest{Format: FormatMP3, Name: "me at the zoo"}, "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusQueued || task.ID != "7b2c5a3e-0000-4000-8000-000000000001" || task.SourceID != nil {
		t.Errorf("task = %+v", task)
	}
}

func TestErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"unprocessable","message":"media is a live stream"}}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "k", nil).GetTask(context.Background(), "x")
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 422 || apiErr.Code != "unprocessable" || apiErr.Message != "media is a live stream" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "k", nil).GetTask(context.Background(), "x")
	apiErr, ok := AsAPIError(err)
	if !ok || apiErr.Code != "http_502" || apiErr.Message != "bad gateway" {
		t.Errorf("err = %v", err)
	}
}

func TestRateLimitRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limited","message":"slow down"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"t1","status":"running","format":"mp4"}`)
	}))
	defer srv.Close()

	task, err := NewClient(srv.URL, "k", nil).GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusRunning || calls.Load() != 2 {
		t.Errorf("status %s after %d calls", task.Status, calls.Load())
	}
}

func TestRateLimitGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := NewClient(srv.URL, "k", nil).GetTask(ctx, "t1")
	if apiErr, ok := AsAPIError(err); !ok || apiErr.StatusCode != 429 {
		t.Errorf("err = %v", err)
	}
}

func TestOpenTaskFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/t1/file" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Disposition", `attachment; filename="Me at the zoo.mp3"`)
		_, _ = io.WriteString(w, "ID3data")
	}))
	defer srv.Close()

	f, err := NewClient(srv.URL, "k", nil).OpenTaskFile(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Body.Close()
	data, _ := io.ReadAll(f.Body)
	if f.Filename != "Me at the zoo.mp3" || f.ContentType != "audio/mpeg" || f.ContentLength != 7 || string(data) != "ID3data" {
		t.Errorf("file = %+v, data %q", f, data)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{"mp3": FormatMP3, "MP4": FormatMP4, "Wav": FormatWAV, "ogg": ""} {
		got, ok := ParseFormat(in)
		if got != want || ok != (want != "") {
			t.Errorf("ParseFormat(%q) = %q, %v", in, got, ok)
		}
	}
}

func TestParseQuality(t *testing.T) {
	for in, want := range map[string]Quality{
		"720": Quality720, "720p": Quality720, "1080P": Quality1080, "BEST": QualityBest,
		"360": Quality360, "480": Quality480, "240": "", "bestp": "", "": "", "4k": "",
	} {
		got, ok := ParseQuality(in)
		if got != want || ok != (want != "") {
			t.Errorf("ParseQuality(%q) = %q, %v", in, got, ok)
		}
	}
}
