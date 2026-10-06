// Package ytdl is a client for the asynchronous task API of ytdl-service.
package ytdl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Format string

const (
	FormatMP3 Format = "mp3"
	FormatMP4 Format = "mp4"
	FormatWAV Format = "wav"
)

func ParseFormat(s string) (Format, bool) {
	switch f := Format(strings.ToLower(s)); f {
	case FormatMP3, FormatMP4, FormatWAV:
		return f, true
	}
	return "", false
}

// Quality is the video resolution of an MP4 download.
type Quality string

const (
	Quality360  Quality = "360"
	Quality480  Quality = "480"
	Quality720  Quality = "720"
	Quality1080 Quality = "1080"
	QualityBest Quality = "best"

	DefaultQuality = Quality480
)

// ParseQuality accepts "720", "720p" or "best" (case-insensitive).
func ParseQuality(s string) (Quality, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if Quality(s) == QualityBest {
		return QualityBest, true
	}
	switch q := Quality(strings.TrimSuffix(s, "p")); q {
	case Quality360, Quality480, Quality720, Quality1080:
		return q, true
	}
	return "", false
}

type TaskStatus string

const (
	StatusQueued    TaskStatus = "queued"
	StatusRunning   TaskStatus = "running"
	StatusSucceeded TaskStatus = "succeeded"
	StatusFailed    TaskStatus = "failed"
)

type CreateTaskRequest struct {
	Format  Format  `json:"format"`
	URL     string  `json:"url,omitempty"`
	Name    string  `json:"name,omitempty"`
	Quality Quality `json:"quality,omitempty"` // video only
}

type Task struct {
	ID            string     `json:"id"`
	Status        TaskStatus `json:"status"`
	Format        Format     `json:"format"`
	SourceID      *string    `json:"source_id"`
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	Query         string     `json:"query"`
	Error         *string    `json:"error"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	StatusURL     string     `json:"status_url"`
	FileURL       string     `json:"file_url"`
	FileAvailable bool       `json:"file_available"`
}

// APIError is the service's {"error": {"code", "message"}} envelope.
type APIError struct {
	StatusCode int
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ytdl: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// AsAPIError unwraps err into an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	ok := errors.As(err, &apiErr)
	return apiErr, ok
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	// maxRetries bounds automatic retries of rate-limited requests.
	maxRetries int
}

func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		// No overall timeout: file bodies can take a while. Requests use contexts.
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		http:       httpClient,
		maxRetries: 3,
	}
}

// CreateTask queues a retrieval. idempotencyKey makes retries return the same task.
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest, idempotencyKey string) (*Task, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/tasks", body, func(h http.Header) {
		h.Set("Content-Type", "application/json")
		if idempotencyKey != "" {
			h.Set("Idempotency-Key", idempotencyKey)
		}
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeTask(resp)
}

func (c *Client) GetTask(ctx context.Context, id string) (*Task, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeTask(resp)
}

// File is an open task result. The caller must close Body.
type File struct {
	Body          io.ReadCloser
	ContentLength int64 // -1 if unknown
	ContentType   string
	Filename      string
}

// OpenTaskFile starts downloading a finished task's file.
func (c *Client) OpenTaskFile(ctx context.Context, id string) (*File, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(id)+"/file", nil, nil)
	if err != nil {
		return nil, err
	}
	f := &File{
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		ContentType:   resp.Header.Get("Content-Type"),
	}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		f.Filename = params["filename"]
	}
	return f, nil
}

// do sends a request and returns a 2xx response, or an *APIError.
// Rate-limited requests are retried after Retry-After.
func (c *Client) do(ctx context.Context, method, path string, body []byte, setHeaders func(http.Header)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		if setHeaders != nil {
			setHeaders(req.Header)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr := readAPIError(resp)
		resp.Body.Close()
		if apiErr.StatusCode != http.StatusTooManyRequests || attempt >= c.maxRetries {
			return nil, apiErr
		}
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func readAPIError(resp *http.Response) *APIError {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	var envelope struct {
		Error *APIError `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	envelope.Error = apiErr
	if err := json.Unmarshal(data, &envelope); err != nil || apiErr.Code == "" {
		apiErr.Code = "http_" + strconv.Itoa(resp.StatusCode)
		apiErr.Message = strings.TrimSpace(string(data))
		if apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
	}
	return apiErr
}

func decodeTask(resp *http.Response) (*Task, error) {
	var t Task
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("ytdl: decode task: %w", err)
	}
	return &t, nil
}
