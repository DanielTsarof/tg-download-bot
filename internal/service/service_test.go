package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"tg-file-downloader/internal/db/migrations"
	"tg-file-downloader/internal/db/sqlc"
	"tg-file-downloader/internal/ytdl"
)

// The pipeline test needs a scratch PostgreSQL database, e.g.
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=test postgres:17-alpine
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/postgres go test ./internal/service
//
// It drops and recreates the schema.
func testPool(t *testing.T) *pgxpool.Pool {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

// fakeYtdl serves tasks that are queued, then running, then finished.
type fakeYtdl struct {
	mu       sync.Mutex
	tasks    map[string]*fakeTask
	created  int
	requests []map[string]any // bodies of POST /v1/tasks
}

type fakeTask struct {
	polls  int
	fail   string // error message for a failing task
	format string
}

func (f *fakeYtdl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks":
		var raw map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		f.requests = append(f.requests, raw)
		var req ytdl.CreateTaskRequest
		_ = json.Unmarshal(body, &req)
		f.created++
		id := fmt.Sprintf("task-%d", f.created)
		t := &fakeTask{format: string(req.Format)}
		if strings.Contains(req.URL+req.Name, "broken") {
			t.fail = "video unavailable"
		}
		f.tasks[id] = t
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "queued", "format": req.Format})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/file"):
		w.Header().Set("Content-Disposition", `attachment; filename="Me at the zoo.mp3"`)
		_, _ = io.WriteString(w, "media-bytes")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
		t := f.tasks[id]
		if t == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
			return
		}
		t.polls++
		resp := map[string]any{"id": id, "format": t.format, "status": "running"}
		if t.polls >= 2 {
			if t.fail != "" {
				resp["status"], resp["error"] = "failed", t.fail
			} else {
				resp["status"], resp["title"], resp["source_id"] = "succeeded", "Me at the zoo", "jNQXAC9IVRw"
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeYtdl) lastRequest() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeYtdl) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// fakeTelegram records Bot API calls.
type fakeTelegram struct {
	mu    sync.Mutex
	calls []tgCall
	msgID int
}

type tgCall struct {
	method string
	fields map[string]string
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	fields := map[string]string{}
	if err := r.ParseMultipartForm(10 << 20); err == nil {
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		for k, fh := range r.MultipartForm.File {
			file, _ := fh[0].Open()
			data, _ := io.ReadAll(file)
			fields[k] = "upload:" + string(data)
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, tgCall{method, fields})
	f.msgID++
	id := f.msgID
	f.mu.Unlock()

	var result any = true
	msg := map[string]any{"message_id": id, "date": 0, "chat": map[string]any{"id": 1, "type": "private"}}
	switch method {
	case "sendMessage":
		result = msg
	case "sendAudio":
		msg["audio"] = map[string]any{"file_id": "audio-file-id", "file_unique_id": "u", "duration": 1}
		result = msg
	case "sendVideo":
		msg["video"] = map[string]any{"file_id": "video-file-id", "file_unique_id": "v", "width": 1, "height": 1, "duration": 1}
		result = msg
	case "sendDocument":
		msg["document"] = map[string]any{"file_id": "document-file-id", "file_unique_id": "d"}
		result = msg
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeTelegram) byMethod(method string) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tgCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTelegram) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func TestPipeline(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	yt := &fakeYtdl{tasks: map[string]*fakeTask{}}
	ytSrv := httptest.NewServer(yt)
	defer ytSrv.Close()
	tg := &fakeTelegram{}
	tgSrv := httptest.NewServer(tg)
	defer tgSrv.Close()

	b, err := bot.New("123:token", bot.WithSkipGetMe(), bot.WithServerURL(tgSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	const storageChat = -100500
	svc := New(pool, ytdl.NewClient(ytSrv.URL, "k", nil), b, Config{
		DefaultDailyLimit: 3, PollInterval: 10 * time.Millisecond, TaskTimeout: 10 * time.Second,
		MaxUploadBytes: 1 << 20, MaxConcurrentUploads: 2, StorageChatID: storageChat,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}

	const userID = 42
	if _, err := q.UpsertUser(ctx, sqlc.UpsertUserParams{ID: userID, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	link := Source{Query: "https://www.youtube.com/watch?v=jNQXAC9IVRw", IsURL: true}
	chat := Target{ChatID: userID, ReplyTo: 7}

	// Pending users can't download.
	if _, err := svc.Submit(ctx, userID, KindCommand, link, ytdl.FormatMP3, chat); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("pending user: err = %v", err)
	}
	if _, err := q.GrantUser(ctx, sqlc.GrantUserParams{ID: userID}); err != nil {
		t.Fatal(err)
	}

	t.Run("download", func(t *testing.T) {
		req, err := svc.Submit(ctx, userID, KindCommand, link, ytdl.FormatMP3, chat)
		if err != nil {
			t.Fatal(err)
		}
		svc.Wait()
		got, _ := q.GetRequest(ctx, req.ID)
		if got.Status != "succeeded" || got.Title != "Me at the zoo" || got.SourceID != "jNQXAC9IVRw" || got.FromCache {
			t.Errorf("request = %+v", got)
		}
		if body := yt.lastRequest(); body["quality"] != nil || got.Quality != "" {
			t.Errorf("audio request carried a quality: %v / %q", body, got.Quality)
		}
		sends := tg.byMethod("sendAudio")
		if len(sends) != 1 || sends[0].fields["audio"] != "upload:media-bytes" || sends[0].fields["chat_id"] != "42" {
			t.Errorf("sendAudio calls = %+v", sends)
		}
		if len(tg.byMethod("deleteMessage")) != 1 {
			t.Error("status message was not deleted")
		}
	})

	t.Run("cache", func(t *testing.T) {
		tg.reset()
		before := yt.createdCount()
		req, err := svc.Submit(ctx, userID, KindCommand, link, ytdl.FormatMP3, chat)
		if err != nil {
			t.Fatal(err)
		}
		svc.Wait()
		got, _ := q.GetRequest(ctx, req.ID)
		if got.Status != "succeeded" || !got.FromCache || yt.createdCount() != before {
			t.Errorf("request = %+v, tasks created %d", got, yt.createdCount()-before)
		}
		sends := tg.byMethod("sendAudio")
		if len(sends) != 1 || sends[0].fields["audio"] != "audio-file-id" {
			t.Errorf("sendAudio calls = %+v", sends)
		}
	})

	t.Run("failed task", func(t *testing.T) {
		tg.reset()
		req, err := svc.Submit(ctx, userID, KindCommand, Source{Query: "broken song"}, ytdl.FormatMP3, chat)
		if err != nil {
			t.Fatal(err)
		}
		svc.Wait()
		got, _ := q.GetRequest(ctx, req.ID)
		if got.Status != "failed" || !strings.Contains(got.Error, "video unavailable") {
			t.Errorf("request = %+v", got)
		}
		edits := tg.byMethod("editMessageText")
		if len(edits) == 0 || !strings.Contains(edits[len(edits)-1].fields["text"], "video unavailable") {
			t.Errorf("failure was not reported: %+v", edits)
		}
	})

	t.Run("video quality", func(t *testing.T) {
		limit := int32(100)
		if _, err := q.SetUserDailyLimit(ctx, sqlc.SetUserDailyLimitParams{ID: userID, DailyLimit: &limit}); err != nil {
			t.Fatal(err)
		}
		video := Source{Query: "https://www.youtube.com/watch?v=video", IsURL: true}
		run := func(src Source) sqlc.Request {
			t.Helper()
			req, err := svc.Submit(ctx, userID, KindCommand, src, ytdl.FormatMP4, chat)
			if err != nil {
				t.Fatal(err)
			}
			svc.Wait()
			got, _ := q.GetRequest(ctx, req.ID)
			return got
		}

		got := run(video) // default
		if got.Quality != "480" || yt.lastRequest()["quality"] != "480" || got.FromCache {
			t.Errorf("default: request %+v, body %v", got, yt.lastRequest())
		}
		before := yt.createdCount()
		hd := video
		hd.Quality = ytdl.Quality720
		got = run(hd) // a cached 480p file must not answer a 720p request
		if got.Quality != "720" || got.FromCache || yt.createdCount() != before+1 || yt.lastRequest()["quality"] != "720" {
			t.Errorf("720p: request %+v, body %v", got, yt.lastRequest())
		}
		got = run(hd)
		if !got.FromCache || yt.createdCount() != before+1 {
			t.Errorf("720p repeat not cached: %+v", got)
		}
		// Back to the default limit (3), which is used up by now.
		if _, err := q.SetUserDailyLimit(ctx, sqlc.SetUserDailyLimitParams{ID: userID}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("limit", func(t *testing.T) {
		_, err := svc.Submit(ctx, userID, KindCommand, link, ytdl.FormatMP3, chat)
		var limitErr *LimitError
		if !errors.As(err, &limitErr) || limitErr.Limit != 3 {
			t.Fatalf("err = %v, want limit error", err)
		}
	})

	t.Run("resume after restart", func(t *testing.T) {
		tg.reset()
		yt.mu.Lock()
		yt.tasks["task-before-restart"] = &fakeTask{format: "wav"}
		yt.mu.Unlock()
		chatID := int64(userID)
		req, err := q.CreateRequest(ctx, sqlc.CreateRequestParams{
			UserID: userID, Kind: KindCommand, Query: "resumed song", Format: "wav", ChatID: &chatID,
		})
		if err != nil {
			t.Fatal(err)
		}
		taskID := "task-before-restart"
		if err := q.SetRequestTask(ctx, sqlc.SetRequestTaskParams{ID: req.ID, TaskID: &taskID, Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Start(ctx); err != nil {
			t.Fatal(err)
		}
		svc.Wait()
		got, _ := q.GetRequest(ctx, req.ID)
		if got.Status != "succeeded" {
			t.Errorf("request = %+v", got)
		}
		if docs := tg.byMethod("sendDocument"); len(docs) != 1 || docs[0].fields["document"] != "upload:media-bytes" {
			t.Errorf("WAV not sent as a document: %+v", docs)
		}
	})

	t.Run("inline", func(t *testing.T) {
		tg.reset()
		limit := int32(10)
		if _, err := q.SetUserDailyLimit(ctx, sqlc.SetUserDailyLimitParams{ID: userID, DailyLimit: &limit}); err != nil {
			t.Fatal(err)
		}
		src := Source{Query: "https://www.youtube.com/watch?v=other", IsURL: true}
		req, err := svc.Submit(ctx, userID, KindInline, src, ytdl.FormatMP3, Target{InlineMessageID: "inline-1"})
		if err != nil {
			t.Fatal(err)
		}
		svc.Wait()
		got, _ := q.GetRequest(ctx, req.ID)
		if got.Status != "succeeded" {
			t.Errorf("request = %+v", got)
		}
		sends := tg.byMethod("sendAudio")
		if len(sends) != 1 || sends[0].fields["chat_id"] != fmt.Sprint(storageChat) {
			t.Errorf("upload did not go to the storage chat: %+v", sends)
		}
		edits := tg.byMethod("editMessageMedia")
		if len(edits) != 1 || edits[0].fields["inline_message_id"] != "inline-1" ||
			!strings.Contains(edits[0].fields["media"], `"audio-file-id"`) {
			t.Errorf("editMessageMedia calls = %+v", edits)
		}
	})
}
