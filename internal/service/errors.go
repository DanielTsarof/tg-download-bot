package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-telegram/bot"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

// TaskFailedError is a task that finished with status "failed".
type TaskFailedError struct {
	Message string
}

func (e *TaskFailedError) Error() string { return "task failed: " + e.Message }

type TimeoutError struct {
	After time.Duration
}

func (e *TimeoutError) Error() string { return fmt.Sprintf("task did not finish within %s", e.After) }

type TooLargeError struct {
	Size, Limit int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("file is %s, upload limit is %s", megabytes(e.Size), megabytes(e.Limit))
}

func megabytes(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// describe turns err into a message for the user and a detail for the history.
func describe(err error) (userMsg, detail string) {
	detail = err.Error()

	var (
		failed   *TaskFailedError
		timeout  *TimeoutError
		tooLarge *TooLargeError
	)
	switch {
	case errors.As(err, &failed):
		msg := failed.Message
		if msg == "" {
			msg = "unknown error"
		}
		return "Download failed: " + msg, detail
	case errors.As(err, &timeout):
		return fmt.Sprintf("The download took longer than %s and was abandoned.", timeout.After), detail
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("The file is too large for Telegram (%s, limit %s). Try MP3 instead of video.",
			megabytes(tooLarge.Size), megabytes(tooLarge.Limit)), detail
	case errors.Is(err, context.DeadlineExceeded):
		return "The download service did not respond in time. Please try again later.", detail
	}

	if apiErr, ok := ytdl.AsAPIError(err); ok {
		switch apiErr.Code {
		case "unprocessable":
			return "This media can't be downloaded: it is too long or a live stream.", detail
		case "upstream_failed":
			return "Couldn't retrieve the media: it may be unavailable or removed.", detail
		case "not_found":
			return "Nothing was found for this request.", detail
		case "bad_request":
			return "The download service rejected the request: " + apiErr.Message, detail
		case "gone":
			return "The downloaded file has expired before it could be sent. Please try again.", detail
		case "rate_limited":
			return "The download service is busy. Please try again in a minute.", detail
		case "unauthorized", "forbidden":
			return "The bot is misconfigured (download service rejected its key). Please tell an admin.", detail
		}
		return "The download service failed. Please try again later.", detail
	}

	if errors.Is(err, bot.ErrorBadRequest) && strings.Contains(strings.ToLower(detail), "too large") {
		return "The file is too large for Telegram.", detail
	}
	return "Something went wrong. Please try again later.", detail
}
