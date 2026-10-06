package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"tg-file-downloader/internal/db/sqlc"
	"tg-file-downloader/internal/ytdl"
)

// CachedFile returns the stored Telegram file for src in format f, if any.
func (s *Service) CachedFile(ctx context.Context, src Source, f ytdl.Format) (*sqlc.FileCache, error) {
	c, err := s.q.GetCachedFile(ctx, sqlc.GetCachedFileParams{LookupKey: src.cacheKey(), Format: string(f)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Remaining returns how many requests u has left today; ok is false for unlimited.
func (s *Service) Remaining(ctx context.Context, u sqlc.User) (remaining int64, ok bool, err error) {
	limit, limited := s.DailyLimit(u)
	if !limited {
		return 0, false, nil
	}
	used, err := s.UsedToday(ctx, u.ID)
	if err != nil {
		return 0, true, err
	}
	return max(int64(limit)-used, 0), true, nil
}

func (s *Service) UsedToday(ctx context.Context, userID int64) (int64, error) {
	return s.q.CountUserRequestsSince(ctx, sqlc.CountUserRequestsSinceParams{
		UserID: userID, CreatedAt: StartOfDay(time.Now()),
	})
}

// RecordCachedInline logs an inline result Telegram already sent from cache.
// The limit was checked when the results were offered, so it isn't enforced here.
func (s *Service) RecordCachedInline(ctx context.Context, userID int64, src Source, f ytdl.Format) error {
	c, err := s.CachedFile(ctx, src, f)
	if err != nil {
		return err
	}
	req, err := s.q.CreateRequest(ctx, sqlc.CreateRequestParams{
		UserID: userID, Kind: KindInline, Query: src.Query, IsUrl: src.IsURL, Format: string(f),
	})
	if err != nil {
		return err
	}
	p := sqlc.SucceedRequestParams{ID: req.ID, FromCache: true}
	if c != nil {
		p.Title, p.SourceID = c.Title, c.SourceID
	}
	return s.q.SucceedRequest(ctx, p)
}
