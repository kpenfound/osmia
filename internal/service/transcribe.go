package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
)

const (
	// maxTranscribeClip bounds one clip's decoded bytes. A request body may
	// carry no more than that clip's base64 encoding and a small envelope.
	maxTranscribeClip = 25 << 20
	// transcribeTimeout bounds the request to the transcription server, from
	// the connection to the last byte of its reply.
	transcribeTimeout = 2 * time.Minute
	// maxTranscribeReply bounds the reply read from the transcription server.
	maxTranscribeReply = 1 << 20
	// transcribeEnvelope is the room a request body has beyond the clip's
	// base64 encoding for its other fields.
	transcribeEnvelope = 4 << 10
	// transcribeWriteMargin is how long the response may take to write after
	// the transcription finishes.
	transcribeWriteMargin = 10 * time.Second
)

// clipExtensions maps the audio media types browsers record to the filename
// extension the transcription server recognizes the format by.
var clipExtensions = map[string]string{
	"audio/webm": "webm", "audio/ogg": "ogg", "audio/opus": "ogg", "audio/mp4": "mp4", "audio/m4a": "m4a", "audio/x-m4a": "m4a",
	"audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/flac": "flac", "audio/x-flac": "flac",
}

var extensionWord = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// clipFormat validates mimeType as an audio media type and returns it in the
// form sent to the transcription server with the matching filename extension.
func clipFormat(mimeType string) (string, string, bool) {
	media, params, err := mime.ParseMediaType(mimeType)
	if err != nil || !strings.HasPrefix(media, "audio/") {
		return "", "", false
	}
	extension, ok := clipExtensions[media]
	if !ok {
		extension = strings.TrimPrefix(strings.TrimPrefix(media, "audio/"), "x-")
		if !extensionWord.MatchString(extension) {
			return "", "", false
		}
	}
	if formatted := mime.FormatMediaType(media, params); formatted != "" {
		return formatted, extension, true
	}
	return media, extension, true
}

func (s *Service) transcribeClipLimit() int64 {
	if s.options.transcribeLimit > 0 {
		return s.options.transcribeLimit
	}
	return maxTranscribeClip
}

func (s *Service) transcribeBound() time.Duration {
	if s.options.transcribeTimeout > 0 {
		return s.options.transcribeTimeout
	}
	return transcribeTimeout
}

// transcribeRequest answers POST /v1/transcribe: it posts the request's clip to
// the configured transcription server and answers with the text. The clip and
// the text are held only for the request; neither reaches a log, a trace or
// an event.
func (s *Service) transcribeRequest(w http.ResponseWriter, r *http.Request) {
	settings := s.current().Transcribe
	if !settings.Enabled() {
		failWith(w, &APIError{Code: TranscriptionOff, Field: "transcribe.url", Message: "transcription is not configured; set url under [transcribe] in the top-level config.toml and reload"})
		return
	}
	// The server's read and write timeouts are sized for small requests; an
	// upload and a transcription take longer.
	bound := s.transcribeBound()
	controls := http.NewResponseController(w)
	_ = controls.SetReadDeadline(time.Now().Add(bound))
	_ = controls.SetWriteDeadline(time.Now().Add(2*bound + transcribeWriteMargin))

	limit := s.transcribeClipLimit()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(base64.StdEncoding.EncodedLen(int(limit)))+transcribeEnvelope))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			failWith(w, &APIError{Code: TooLarge, Message: "the recording is too large to transcribe"})
		} else {
			fail(w, Malformed)
		}
		return
	}
	var req TranscribeRequest
	if !decodeBody(w, body, &req) {
		return
	}
	body = nil
	mimeType, extension, ok := clipFormat(req.MIMEType)
	if !ok {
		failWith(w, &APIError{Code: Validation, Message: "mime_type must be an audio media type such as audio/webm"})
		return
	}
	if int64(base64.StdEncoding.DecodedLen(len(req.Audio))) > limit+2 {
		failWith(w, &APIError{Code: TooLarge, Message: "the recording is too large to transcribe"})
		return
	}
	clip, err := base64.StdEncoding.DecodeString(req.Audio)
	req.Audio = ""
	if err != nil {
		failWith(w, &APIError{Code: Malformed, Message: "audio must be base64"})
		return
	}
	if len(clip) == 0 {
		failWith(w, &APIError{Code: Validation, Message: "the recording is empty"})
		return
	}
	if int64(len(clip)) > limit {
		failWith(w, &APIError{Code: TooLarge, Message: "the recording is too large to transcribe"})
		return
	}
	text, failure := s.forwardClip(r.Context(), settings.URL, settings.Model, settings.APIKeyEnv, mimeType, extension, clip, bound)
	if failure != nil {
		if r.Context().Err() == nil {
			failWith(w, failure)
		}
		return
	}
	respond(w, http.StatusOK, TranscribeResponse{Text: text})
}

// forwardClip posts clip to the OpenAI-compatible transcription endpoint at
// url and returns its text. A failure names what went wrong without the
// clip, the text, the key or the server's reply.
func (s *Service) forwardClip(ctx context.Context, url, model, keyEnv, mimeType, extension string, clip []byte, bound time.Duration) (string, *APIError) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": "audio." + extension}))
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err == nil {
		_, err = part.Write(clip)
	}
	if err == nil {
		err = writer.WriteField("model", model)
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		return "", &APIError{Code: Internal, Message: "cannot prepare the transcription request"}
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &form)
	if err != nil {
		return "", &APIError{Code: Internal, Message: "cannot prepare the transcription request"}
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	getenv := s.options.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if key := getenv(keyEnv); key != "" {
		// The key is registered before it can reach any record.
		redact.Register(key)
		request.Header.Set("Authorization", "Bearer "+key)
	}
	// A redirect would carry the clip to a server the owner did not name.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", s.transcriptionFailed("timed out", "the transcription server did not answer in time", err)
		}
		return "", s.transcriptionFailed("unreachable", "the transcription server could not be reached", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", s.transcriptionFailed("status "+strconv.Itoa(response.StatusCode), "the transcription server refused the request with status "+strconv.Itoa(response.StatusCode), nil)
	}
	reply, err := io.ReadAll(io.LimitReader(response.Body, maxTranscribeReply+1))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", s.transcriptionFailed("timed out", "the transcription server did not answer in time", err)
		}
		return "", s.transcriptionFailed("unreadable reply", "the transcription server's reply could not be read", err)
	}
	var out struct {
		Text *string `json:"text"`
	}
	if len(reply) > maxTranscribeReply || json.Unmarshal(reply, &out) != nil || out.Text == nil {
		return "", s.transcriptionFailed("unreadable reply", "the transcription server's reply was not a transcription", nil)
	}
	return strings.TrimSpace(*out.Text), nil
}

// transcriptionFailed logs why a transcription failed, without the clip or
// any text, and returns the error to answer with.
func (s *Service) transcriptionFailed(reason, message string, cause error) *APIError {
	attrs := []any{"reason", reason}
	if cause != nil {
		attrs = append(attrs, "error", redact.Secrets(cause.Error(), redact.KnownSecrets()...))
	}
	s.log().Warn("transcription failed", attrs...)
	return &APIError{Code: TranscriptionFailed, Message: message}
}
