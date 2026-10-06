package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tg-file-downloader/internal/db/sqlc"
	"tg-file-downloader/internal/ytdl"
)

// MediaKind is how a file is sent to Telegram.
type MediaKind string

const (
	MediaAudio    MediaKind = "audio"
	MediaVideo    MediaKind = "video"
	MediaDocument MediaKind = "document"
)

// mediaKindFor picks the Telegram message type: the audio player only takes
// MP3/M4A, so WAV goes out as a document.
func mediaKindFor(f ytdl.Format) MediaKind {
	switch f {
	case ytdl.FormatMP3:
		return MediaAudio
	case ytdl.FormatMP4:
		return MediaVideo
	}
	return MediaDocument
}

type phase int

const (
	phaseQueued phase = iota
	phaseDownloading
	phaseUploading
)

func statusText(req sqlc.Request, p phase) string {
	var prefix string
	switch p {
	case phaseQueued:
		prefix = "⏳ Queued"
	case phaseDownloading:
		prefix = "⬇️ Downloading"
	case phaseUploading:
		prefix = "📤 Sending"
	}
	return fmt.Sprintf("%s [%s]: %s", prefix, strings.ToUpper(req.Format), req.Query)
}

func replyTo(messageID int) *models.ReplyParameters {
	return &models.ReplyParameters{MessageID: messageID, AllowSendingWithoutReply: true}
}

// setProgress updates the status message (chat) or the placeholder (inline).
func (s *Service) setProgress(ctx context.Context, req sqlc.Request, p phase) {
	text := statusText(req, p)
	var err error
	switch {
	case req.InlineMessageID != nil:
		_, err = s.tg.EditMessageText(ctx, &bot.EditMessageTextParams{
			InlineMessageID: *req.InlineMessageID, Text: text,
		})
	case req.ChatID != nil && req.StatusMessageID != nil:
		_, err = s.tg.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID: *req.ChatID, MessageID: int(*req.StatusMessageID), Text: text,
		})
	}
	if err != nil && !isNotModified(err) {
		s.log.Debug("update progress", "request_id", req.ID, "err", err)
	}
}

func (s *Service) deleteStatusMessage(ctx context.Context, req sqlc.Request) {
	if req.ChatID == nil || req.StatusMessageID == nil {
		return
	}
	if _, err := s.tg.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID: *req.ChatID, MessageID: int(*req.StatusMessageID),
	}); err != nil {
		s.log.Debug("delete status message", "request_id", req.ID, "err", err)
	}
}

func (s *Service) notifyFailure(ctx context.Context, req sqlc.Request, userMsg string, log *slog.Logger) {
	text := fmt.Sprintf("❌ [%s] %s\n%s", strings.ToUpper(req.Format), req.Query, userMsg)
	var err error
	switch {
	case req.InlineMessageID != nil:
		_, err = s.tg.EditMessageText(ctx, &bot.EditMessageTextParams{InlineMessageID: *req.InlineMessageID, Text: text})
	case req.ChatID != nil && req.StatusMessageID != nil:
		_, err = s.tg.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID: *req.ChatID, MessageID: int(*req.StatusMessageID), Text: text,
		})
		if err != nil {
			_, err = s.tg.SendMessage(ctx, &bot.SendMessageParams{ChatID: *req.ChatID, Text: text})
		}
	case req.ChatID != nil:
		_, err = s.tg.SendMessage(ctx, &bot.SendMessageParams{ChatID: *req.ChatID, Text: text})
	}
	if err != nil {
		log.Warn("notify failure", "err", err)
	}
}

// deliverFileID sends an already uploaded file to the request's target.
func (s *Service) deliverFileID(ctx context.Context, req sqlc.Request, kind MediaKind, fileID, title string) error {
	if req.InlineMessageID != nil {
		return s.editInline(ctx, *req.InlineMessageID, kind, fileID, title)
	}
	if req.ChatID == nil {
		return errors.New("request has no target")
	}
	if _, err := s.send(ctx, *req.ChatID, kind, &models.InputFileString{Data: fileID}, title); err != nil {
		return err
	}
	s.deleteStatusMessage(ctx, req)
	return nil
}

// upload sends a new file to chatID and returns its file_id and the kind
// Telegram stored it as (a video it can't process comes back as a document).
func (s *Service) upload(ctx context.Context, chatID int64, kind MediaKind, filename string, r io.Reader, title string) (string, MediaKind, error) {
	msg, err := s.send(ctx, chatID, kind, &models.InputFileUpload{Filename: filename, Data: r}, title)
	if err != nil {
		return "", "", err
	}
	switch {
	case msg.Audio != nil:
		return msg.Audio.FileID, MediaAudio, nil
	case msg.Video != nil:
		return msg.Video.FileID, MediaVideo, nil
	case msg.Document != nil:
		return msg.Document.FileID, MediaDocument, nil
	}
	return "", "", errors.New("telegram returned no file in the sent message")
}

func (s *Service) send(ctx context.Context, chatID int64, kind MediaKind, file models.InputFile, title string) (*models.Message, error) {
	caption := truncate(title, 1024)
	switch kind {
	case MediaAudio:
		return s.tg.SendAudio(ctx, &bot.SendAudioParams{ChatID: chatID, Audio: file, Title: title, Caption: caption})
	case MediaVideo:
		return s.tg.SendVideo(ctx, &bot.SendVideoParams{ChatID: chatID, Video: file, Caption: caption, SupportsStreaming: true})
	default:
		return s.tg.SendDocument(ctx, &bot.SendDocumentParams{ChatID: chatID, Document: file, Caption: caption})
	}
}

// editInline replaces an inline placeholder with the file. Inline messages
// can't take new uploads, so fileID must already be on Telegram's servers.
func (s *Service) editInline(ctx context.Context, inlineMessageID string, kind MediaKind, fileID, title string) error {
	_, err := s.tg.EditMessageMedia(ctx, &bot.EditMessageMediaParams{
		InlineMessageID: inlineMessageID,
		Media:           InputMedia(kind, fileID, title),
	})
	return err
}

// InputMedia builds the edit payload for a stored file.
func InputMedia(kind MediaKind, fileID, title string) models.InputMedia {
	caption := truncate(title, 1024)
	switch kind {
	case MediaAudio:
		return &models.InputMediaAudio{Media: fileID, Title: title, Caption: caption}
	case MediaVideo:
		return &models.InputMediaVideo{Media: fileID, Caption: caption, SupportsStreaming: true}
	default:
		return &models.InputMediaDocument{Media: fileID, Caption: caption}
	}
}

func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

func isBadFileID(err error) bool {
	if err == nil || !errors.Is(err, bot.ErrorBadRequest) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "file identifier") || strings.Contains(msg, "file_id") ||
		strings.Contains(msg, "can't use file of type")
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func trimExt(name string) string {
	return strings.TrimSuffix(name, path.Ext(name))
}

// safeFilename keeps a title usable as a file name.
func safeFilename(title string) string {
	name := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			return '_'
		}
		return r
	}, title)
	name = truncate(strings.TrimSpace(name), 100)
	if name == "" {
		return "file"
	}
	return name
}
