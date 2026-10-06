// Package service runs download requests: it records them, drives the
// ytdl-service task API (create → poll → fetch) and delivers the result to Telegram.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"tg-file-downloader/internal/db/sqlc"
	"tg-file-downloader/internal/ytdl"
)

const (
	RoleUser  = "user"
	RoleAdmin = "admin"

	StatusPending = "pending"
	StatusActive  = "active"
	StatusBlocked = "blocked"

	KindCommand = "command"
	KindInline  = "inline"
)

// ErrNoAccess is returned for users who are not active.
var ErrNoAccess = errors.New("access not granted")

// LimitError is returned when a user has used up today's requests.
type LimitError struct {
	Limit int32
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("daily limit of %d requests reached", e.Limit)
}

type Config struct {
	DefaultDailyLimit    int32
	PollInterval         time.Duration
	TaskTimeout          time.Duration
	MaxUploadBytes       int64
	MaxConcurrentUploads int
	StorageChatID        int64
}

// Target is where the result goes: a chat, or an inline message.
type Target struct {
	ChatID          int64
	ReplyTo         int // message to reply to with the status message
	InlineMessageID string
}

type Service struct {
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	ytdl    *ytdl.Client
	tg      *bot.Bot
	cfg     Config
	log     *slog.Logger
	uploads chan struct{}

	ctx context.Context // root context of request goroutines, set by Start
	wg  sync.WaitGroup
}

func New(pool *pgxpool.Pool, yt *ytdl.Client, tg *bot.Bot, cfg Config, log *slog.Logger) *Service {
	return &Service{
		pool:    pool,
		q:       sqlc.New(pool),
		ytdl:    yt,
		tg:      tg,
		cfg:     cfg,
		log:     log,
		uploads: make(chan struct{}, cfg.MaxConcurrentUploads),
		ctx:     context.Background(),
	}
}

func (s *Service) Queries() *sqlc.Queries { return s.q }

func (s *Service) InlineEnabled() bool { return s.cfg.StorageChatID != 0 }

// Start sets the context request goroutines run under and resumes requests
// left unfinished by a previous run. Cancelling ctx stops them without
// failing them, so the next start picks them up again.
func (s *Service) Start(ctx context.Context) error {
	s.ctx = ctx
	reqs, err := s.q.ListUnfinishedRequests(ctx)
	if err != nil {
		return fmt.Errorf("list unfinished requests: %w", err)
	}
	for _, req := range reqs {
		s.log.Info("resuming request", "request_id", req.ID, "task_id", deref(req.TaskID))
		s.spawn(req)
	}
	return nil
}

// Wait blocks until all request goroutines have returned.
func (s *Service) Wait() { s.wg.Wait() }

// StartOfDay is the beginning of the current limit window (midnight UTC).
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// DailyLimit returns the user's effective limit; ok is false for unlimited (admins).
func (s *Service) DailyLimit(u sqlc.User) (limit int32, ok bool) {
	if u.Role == RoleAdmin {
		return 0, false
	}
	if u.DailyLimit != nil {
		return *u.DailyLimit, true
	}
	return s.cfg.DefaultDailyLimit, true
}

// Submit records a request and starts processing it in the background.
func (s *Service) Submit(ctx context.Context, userID int64, kind string, src Source, f ytdl.Format, t Target) (sqlc.Request, error) {
	req, err := s.createRequest(ctx, userID, kind, src, f, t)
	if err != nil {
		return req, err
	}
	if t.ChatID != 0 {
		params := &bot.SendMessageParams{ChatID: t.ChatID, Text: statusText(req, phaseQueued)}
		if t.ReplyTo != 0 {
			params.ReplyParameters = replyTo(t.ReplyTo)
		}
		if msg, err := s.tg.SendMessage(ctx, params); err != nil {
			s.log.Warn("send status message", "request_id", req.ID, "err", err)
		} else {
			id := int32(msg.ID)
			req.StatusMessageID = &id
			if err := s.q.SetRequestStatusMessage(ctx, sqlc.SetRequestStatusMessageParams{ID: req.ID, StatusMessageID: &id}); err != nil {
				s.log.Warn("save status message", "request_id", req.ID, "err", err)
			}
		}
	}
	s.spawn(req)
	return req, nil
}

// createRequest checks access and the daily limit and inserts the request,
// atomically: the user row is locked so concurrent requests can't overshoot.
func (s *Service) createRequest(ctx context.Context, userID int64, kind string, src Source, f ytdl.Format, t Target) (sqlc.Request, error) {
	var req sqlc.Request
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		u, err := q.GetUserForUpdate(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoAccess
		}
		if err != nil {
			return err
		}
		if u.Status != StatusActive {
			return ErrNoAccess
		}
		if limit, limited := s.DailyLimit(u); limited {
			used, err := q.CountUserRequestsSince(ctx, sqlc.CountUserRequestsSinceParams{
				UserID: userID, CreatedAt: StartOfDay(time.Now()),
			})
			if err != nil {
				return err
			}
			if used >= int64(limit) {
				return &LimitError{Limit: limit}
			}
		}
		req, err = q.CreateRequest(ctx, sqlc.CreateRequestParams{
			UserID:          userID,
			Kind:            kind,
			Query:           src.Query,
			IsUrl:           src.IsURL,
			Format:          string(f),
			ChatID:          nonZero(t.ChatID),
			InlineMessageID: nonEmpty(t.InlineMessageID),
		})
		return err
	})
	return req, err
}

func (s *Service) spawn(req sqlc.Request) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.process(s.ctx, req)
	}()
}

func (s *Service) process(ctx context.Context, req sqlc.Request) {
	log := s.log.With("request_id", req.ID, "user_id", req.UserID, "format", req.Format)
	err := s.execute(ctx, req, log)
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		log.Info("request interrupted by shutdown; it will resume on the next start")
		return
	}
	userMsg, detail := describe(err)
	log.Warn("request failed", "err", err)
	if dbErr := s.q.FailRequest(ctx, sqlc.FailRequestParams{ID: req.ID, Error: detail}); dbErr != nil {
		log.Error("record failure", "err", dbErr)
	}
	s.notifyFailure(ctx, req, userMsg, log)
}

func (s *Service) execute(ctx context.Context, req sqlc.Request, log *slog.Logger) error {
	src := Source{Query: req.Query, IsURL: req.IsUrl}
	format := ytdl.Format(req.Format)
	key := src.cacheKey()

	// Files uploaded before are re-sent by file_id without touching the service.
	if req.TaskID == nil {
		cached, err := s.q.GetCachedFile(ctx, sqlc.GetCachedFileParams{LookupKey: key, Format: req.Format})
		switch {
		case err == nil:
			err := s.deliverFileID(ctx, req, MediaKind(cached.MediaKind), cached.TgFileID, cached.Title)
			if err == nil {
				log.Info("served from cache")
				return s.q.SucceedRequest(ctx, sqlc.SucceedRequestParams{
					ID: req.ID, Title: cached.Title, SourceID: cached.SourceID, FromCache: true,
				})
			}
			if !isBadFileID(err) {
				return err
			}
			log.Warn("cached file_id rejected; downloading again", "err", err)
			_ = s.q.DeleteCachedFile(ctx, sqlc.DeleteCachedFileParams{LookupKey: key, Format: req.Format})
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		task, err := s.ytdl.CreateTask(ctx, src.taskRequest(format), fmt.Sprintf("tg-request-%d", req.ID))
		if err != nil {
			return fmt.Errorf("create task: %w", err)
		}
		req.TaskID = &task.ID
		req.Status = dbStatus(task.Status)
		if err := s.q.SetRequestTask(ctx, sqlc.SetRequestTaskParams{ID: req.ID, TaskID: req.TaskID, Status: req.Status}); err != nil {
			return err
		}
		log.Info("task created", "task_id", task.ID)
	}
	log = log.With("task_id", *req.TaskID)

	task, err := s.waitTask(ctx, req, log)
	if err != nil {
		return err
	}
	if task.Status == ytdl.StatusFailed {
		return &TaskFailedError{Message: deref(task.Error)}
	}

	// Transfers are bounded: the Telegram client buffers each upload in memory.
	select {
	case s.uploads <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.uploads }()

	s.setProgress(ctx, req, phaseUploading)
	file, err := s.ytdl.OpenTaskFile(ctx, task.ID)
	if err != nil {
		return fmt.Errorf("open task file: %w", err)
	}
	defer file.Body.Close()
	if file.ContentLength > s.cfg.MaxUploadBytes {
		return &TooLargeError{Size: file.ContentLength, Limit: s.cfg.MaxUploadBytes}
	}

	title := firstNonEmpty(task.Title, trimExt(file.Filename), req.Query)
	filename := file.Filename
	if filename == "" {
		filename = safeFilename(title) + "." + req.Format
	}

	kind := mediaKindFor(format)
	chatID := s.cfg.StorageChatID
	if req.ChatID != nil {
		chatID = *req.ChatID
	} else if chatID == 0 {
		return errors.New("inline delivery needs STORAGE_CHAT_ID")
	}
	fileID, sentKind, err := s.upload(ctx, chatID, kind, filename, file.Body, title)
	if err != nil {
		return fmt.Errorf("upload to telegram: %w", err)
	}

	sourceID := deref(task.SourceID)
	if err := s.q.PutCachedFile(ctx, sqlc.PutCachedFileParams{
		LookupKey: key, Format: req.Format, TgFileID: fileID, MediaKind: string(sentKind),
		Title: title, SourceID: sourceID,
	}); err != nil {
		log.Warn("cache file_id", "err", err)
	}

	if req.InlineMessageID != nil {
		if err := s.editInline(ctx, *req.InlineMessageID, sentKind, fileID, title); err != nil {
			return fmt.Errorf("edit inline message: %w", err)
		}
	} else {
		s.deleteStatusMessage(ctx, req)
	}
	log.Info("request succeeded", "title", title)
	return s.q.SucceedRequest(ctx, sqlc.SucceedRequestParams{ID: req.ID, Title: title, SourceID: sourceID})
}

// waitTask polls the task until it finishes or the request times out.
func (s *Service) waitTask(ctx context.Context, req sqlc.Request, log *slog.Logger) (*ytdl.Task, error) {
	pollCtx, cancel := context.WithDeadline(ctx, req.CreatedAt.Add(s.cfg.TaskTimeout))
	defer cancel()

	status := req.Status
	for {
		wait := s.cfg.PollInterval
		task, err := s.ytdl.GetTask(pollCtx, *req.TaskID)
		switch {
		case err == nil:
			switch task.Status {
			case ytdl.StatusSucceeded, ytdl.StatusFailed:
				return task, nil
			}
			if st := dbStatus(task.Status); st != status {
				status = st
				if err := s.q.SetRequestStatus(ctx, sqlc.SetRequestStatusParams{ID: req.ID, Status: st}); err != nil {
					log.Warn("update status", "err", err)
				}
				if task.Status == ytdl.StatusRunning {
					s.setProgress(ctx, req, phaseDownloading)
				}
			}
		case pollCtx.Err() != nil:
			// Handled below.
		default:
			apiErr, ok := ytdl.AsAPIError(err)
			if ok && apiErr.StatusCode < 500 && apiErr.StatusCode != 429 {
				return nil, fmt.Errorf("poll task: %w", err)
			}
			log.Warn("poll task; retrying", "err", err)
			if ok && apiErr.RetryAfter > wait {
				wait = apiErr.RetryAfter
			}
		}

		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &TimeoutError{After: s.cfg.TaskTimeout}
		case <-time.After(wait):
		}
	}
}

func dbStatus(st ytdl.TaskStatus) string {
	switch st {
	case ytdl.StatusRunning, ytdl.StatusSucceeded, ytdl.StatusFailed:
		return string(st)
	}
	return string(ytdl.StatusQueued)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func nonZero(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
