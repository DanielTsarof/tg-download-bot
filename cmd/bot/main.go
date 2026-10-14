package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/bot"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/config"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/migrations"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/sqlc"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/service"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectDB(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := migrate(ctx, pool, log); err != nil {
		return err
	}
	q := sqlc.New(pool)
	for _, id := range cfg.AdminIDs {
		if err := q.EnsureAdmin(ctx, id); err != nil {
			return fmt.Errorf("bootstrap admin %d: %w", id, err)
		}
	}

	// The handler needs the bot (through the service), and the bot needs the
	// handler, so the bot's callbacks reach it through this variable.
	var h *bot.Handler
	opts := []tgbot.Option{
		tgbot.WithMiddlewares(func(next tgbot.HandlerFunc) tgbot.HandlerFunc {
			return func(ctx context.Context, b *tgbot.Bot, upd *models.Update) {
				h.UserMiddleware(next)(ctx, b, upd)
			}
		}),
		tgbot.WithDefaultHandler(func(ctx context.Context, b *tgbot.Bot, upd *models.Update) {
			h.Handle(ctx, b, upd)
		}),
		tgbot.WithAllowedUpdates(tgbot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateCallbackQuery,
			models.AllowedUpdateInlineQuery,
			models.AllowedUpdateChosenInlineResult,
		}),
		tgbot.WithErrorsHandler(func(err error) { log.Warn("telegram", "err", err) }),
	}
	if cfg.TelegramAPIURL != "" {
		opts = append(opts, tgbot.WithServerURL(cfg.TelegramAPIURL))
	}
	b, err := tgbot.New(cfg.TelegramToken, opts...)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	me, err := b.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram getMe: %w", err)
	}

	svc := service.New(pool, ytdl.NewClient(cfg.YtdlBaseURL, cfg.YtdlAPIKey, nil), b, service.Config{
		DefaultDailyLimit:    cfg.DefaultDailyLimit,
		PollInterval:         cfg.PollInterval,
		TaskTimeout:          cfg.TaskTimeout,
		MaxUploadBytes:       cfg.MaxUploadBytes,
		MaxConcurrentUploads: cfg.MaxConcurrentUploads,
		StorageChatID:        cfg.StorageChatID,
	}, log)
	h = bot.New(svc, me.Username, log)

	if cfg.StorageChatID == 0 {
		log.Warn("STORAGE_CHAT_ID is not set: inline downloads are disabled")
	} else if _, err := b.GetChat(ctx, &tgbot.GetChatParams{ChatID: cfg.StorageChatID}); err != nil {
		log.Warn("storage chat is not reachable; inline downloads will fail", "chat_id", cfg.StorageChatID, "err", err)
	}

	h.SetCommands(ctx, b)
	if err := svc.Start(ctx); err != nil {
		return err
	}

	log.Info("bot started", "username", me.Username)
	b.Start(ctx) // blocks until ctx is cancelled

	log.Info("shutting down; waiting for running requests to stop")
	done := make(chan struct{})
	go func() { svc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Warn("requests did not stop in time")
	}
	return nil
}

func connectDB(ctx context.Context, url string, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	// The database may still be starting next to us.
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if attempt == 10 || ctx.Err() != nil {
			pool.Close()
			return nil, fmt.Errorf("database ping: %w", err)
		}
		log.Warn("database not ready; retrying", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "took", r.Duration)
	}
	return nil
}
