// Package config loads the bot configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	TelegramToken  string
	TelegramAPIURL string // optional local Bot API server
	StorageChatID  int64  // chat that holds uploads for inline mode; 0 disables inline downloads

	YtdlBaseURL string
	YtdlAPIKey  string

	DatabaseURL string

	AdminIDs          []int64
	DefaultDailyLimit int32

	PollInterval         time.Duration
	TaskTimeout          time.Duration
	MaxConcurrentUploads int
	MaxUploadBytes       int64
}

// Load reads .env (if present) and the process environment.
func Load() (*Config, error) {
	// A missing .env is fine: in Docker the variables come from env_file.
	_ = godotenv.Load()

	c := &Config{
		TelegramToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramAPIURL: os.Getenv("TELEGRAM_API_URL"),
		YtdlBaseURL:    strings.TrimRight(getenv("YTDL_BASE_URL", "http://ytdl-service-app-1:8080"), "/"),
		YtdlAPIKey:     os.Getenv("YTDL_API_KEY"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
	}

	var errs []error
	if c.TelegramToken == "" {
		errs = append(errs, errors.New("TELEGRAM_BOT_TOKEN is required"))
	}
	if c.YtdlAPIKey == "" {
		errs = append(errs, errors.New("YTDL_API_KEY is required"))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	var err error
	if c.StorageChatID, err = getInt64("STORAGE_CHAT_ID", 0); err != nil {
		errs = append(errs, err)
	}
	for _, s := range strings.Split(os.Getenv("ADMIN_IDS"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("ADMIN_IDS: invalid ID %q", s))
			continue
		}
		c.AdminIDs = append(c.AdminIDs, id)
	}
	limit, err := getInt64("DEFAULT_DAILY_LIMIT", 20)
	if err != nil {
		errs = append(errs, err)
	}
	c.DefaultDailyLimit = int32(limit)
	if c.PollInterval, err = getDuration("POLL_INTERVAL", 3*time.Second); err != nil {
		errs = append(errs, err)
	}
	if c.TaskTimeout, err = getDuration("TASK_TIMEOUT", 20*time.Minute); err != nil {
		errs = append(errs, err)
	}
	maxUploads, err := getInt64("MAX_CONCURRENT_UPLOADS", 4)
	if err != nil {
		errs = append(errs, err)
	}
	c.MaxConcurrentUploads = max(int(maxUploads), 1)

	// The public Bot API accepts uploads up to 50 MB; a local server up to 2000 MB.
	defaultUpload := int64(50 << 20)
	if c.TelegramAPIURL != "" {
		defaultUpload = 2000 << 20
	}
	maxMB, err := getInt64("MAX_UPLOAD_MB", defaultUpload>>20)
	if err != nil {
		errs = append(errs, err)
	}
	c.MaxUploadBytes = maxMB << 20

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return d, nil
}
