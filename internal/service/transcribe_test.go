package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	clipBytes   = "RECORDED-AUDIO-BYTES-0123456789"
	whisperText = "hello from the fake whisper server"
	whisperKey  = "whisper-secret-key-9d41"
)

// whisperCall is one request the fake Whisper server received.
type whisperCall struct {
	path, auth, mediaType string
	file                  []byte
	fileType, filename    string
	model                 string
	fields                []string
}

// fakeWhisper is an OpenAI-compatible transcription server that records what
// it receives. Its handler answers {"text": whisperText} unless set.
type fakeWhisper struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []whisperCall
	handler func(http.ResponseWriter, *http.Request)
}

func newFakeWhisper(t *testing.T) *fakeWhisper {
	t.Helper()
	f := &fakeWhisper{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := whisperCall{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		call.mediaType = strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]
		if err := r.ParseMultipartForm(64 << 20); err == nil {
			call.model = r.FormValue("model")
			for name := range r.MultipartForm.Value {
				call.fields = append(call.fields, name)
			}
			for name, headers := range r.MultipartForm.File {
				call.fields = append(call.fields, name)
				if name != "file" {
					continue
				}
				header := headers[0]
				call.filename, call.fileType = header.Filename, header.Header.Get("Content-Type")
				file, _ := header.Open()
				call.file, _ = io.ReadAll(file)
				file.Close()
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		handler := f.handler
		f.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": " " + whisperText + "\n"})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWhisper) received() []whisperCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]whisperCall(nil), f.calls...)
}

func (f *fakeWhisper) url() string { return f.Server.URL + "/v1/audio/transcriptions" }

// withTranscribe appends a [transcribe] table to the fixture's top-level file.
func withTranscribe(t *testing.T, opts Options, body string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte("\n[transcribe]\n"+body)...), 0600))
}

// transcribeFixture starts a service whose [transcribe] table points at a
// fake Whisper server and whose key comes from keys. It captures the log.
func transcribeFixture(t *testing.T, keys map[string]string, tune func(*Options)) (*fakeWhisper, *Service, *Client, Options, *syncBuffer) {
	t.Helper()
	whisper := newFakeWhisper(t)
	opts := fixture(t)
	withTranscribe(t, opts, "url = \""+whisper.url()+"\"\n")
	opts.getenv = func(name string) string { return keys[name] }
	var log syncBuffer
	opts.Logger = slog.New(slog.NewJSONHandler(&log, nil))
	if tune != nil {
		tune(&opts)
	}
	s, c := start(t, opts)
	return whisper, s, c, opts, &log
}

func clipRequest(mimeType string, audio []byte) TranscribeRequest {
	return TranscribeRequest{MIMEType: mimeType, Audio: base64.StdEncoding.EncodeToString(audio)}
}

func transcribeError(t *testing.T, err error, code Code) *APIError {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("want a %s error, got %v", code, err)
	}
	return api
}

// A clip goes to the configured server as multipart with its MIME type, a
// matching filename extension and the configured model, and the server's text
// comes back; the bearer key is sent only when its variable is set.
func TestTranscribeForwardsTheClipAsMultipart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mimeType, wantType, extension string
	}{
		{"webm with codecs", "audio/webm;codecs=opus", "audio/webm; codecs=opus", "webm"},
		{"ogg", "audio/ogg", "audio/ogg", "ogg"},
		{"mp4", "audio/mp4", "audio/mp4", "mp4"},
		{"wav", "audio/x-wav", "audio/x-wav", "wav"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			whisper, _, c, _, _ := transcribeFixture(t, nil, nil)
			out, err := c.Transcribe(context.Background(), clipRequest(tc.mimeType, []byte(clipBytes)))
			must(t, err)
			if out.Text != whisperText {
				t.Fatalf("text %q", out.Text)
			}
			calls := whisper.received()
			if len(calls) != 1 {
				t.Fatalf("the server received %d requests", len(calls))
			}
			call := calls[0]
			if call.path != "/v1/audio/transcriptions" || call.mediaType != "multipart/form-data" || string(call.file) != clipBytes ||
				call.fileType != tc.wantType || call.filename != "audio."+tc.extension || call.model != "large-v3-turbo" {
				t.Fatalf("forwarded %+v", call)
			}
			if call.auth != "" {
				t.Fatalf("an Authorization header was sent without a key: %q", call.auth)
			}
			if len(call.fields) != 2 {
				t.Fatalf("form fields %v", call.fields)
			}
		})
	}
}

func TestTranscribeSendsTheBearerKeyOnlyWhenSet(t *testing.T) {
	t.Parallel()
	whisper, _, c, _, _ := transcribeFixture(t, map[string]string{"WHISPER_API_KEY": whisperKey}, nil)
	_, err := c.Transcribe(context.Background(), clipRequest("audio/webm", []byte(clipBytes)))
	must(t, err)
	if calls := whisper.received(); len(calls) != 1 || calls[0].auth != "Bearer "+whisperKey {
		t.Fatalf("calls %+v", calls)
	}

	// The key comes from the variable api_key_env names.
	whisper = newFakeWhisper(t)
	opts := fixture(t)
	withTranscribe(t, opts, "url = \""+whisper.url()+"\"\nmodel = \"whisper-1\"\napi_key_env = \"OTHER_KEY\"\n")
	opts.getenv = func(name string) string {
		return map[string]string{"WHISPER_API_KEY": "wrong", "OTHER_KEY": "other-key"}[name]
	}
	_, c = start(t, opts)
	_, err = c.Transcribe(context.Background(), clipRequest("audio/webm", []byte(clipBytes)))
	must(t, err)
	if calls := whisper.received(); len(calls) != 1 || calls[0].auth != "Bearer other-key" || calls[0].model != "whisper-1" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestTranscribeWhenOffContactsNothing(t *testing.T) {
	t.Parallel()
	whisper := newFakeWhisper(t)
	_, c := start(t, fixture(t))
	_, err := c.Transcribe(context.Background(), clipRequest("audio/webm", []byte(clipBytes)))
	api := transcribeError(t, err, TranscriptionOff)
	if !strings.Contains(api.Message, "transcription is not configured") || api.Field != "transcribe.url" {
		t.Fatalf("off error %+v", api)
	}
	if calls := whisper.received(); len(calls) != 0 {
		t.Fatalf("the server received %+v", calls)
	}
}

// A reload that changes [transcribe] applies to the next transcription.
func TestTranscribeFollowsAReload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first, _, c, opts, _ := transcribeFixture(t, nil, nil)
	second := newFakeWhisper(t)
	second.handler = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"text":"from the second server"}`) }
	files := configFiles(t, opts)

	// A reload that fails leaves the loaded settings in force.
	files.write(t, strings.Replace(files.topText, first.url(), "ftp://whisper.example/v1/audio/transcriptions", 1), files.projectText)
	reloadFails(t, c, files.top, "transcribe.url")
	out, err := c.Transcribe(ctx, clipRequest("audio/webm", []byte(clipBytes)))
	must(t, err)
	if out.Text != whisperText || len(first.received()) != 1 {
		t.Fatalf("after a failed reload: %q %d", out.Text, len(first.received()))
	}

	files.write(t, strings.Replace(files.topText, first.url(), second.url(), 1)+"model = \"whisper-1\"\n", files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	out, err = c.Transcribe(ctx, clipRequest("audio/webm", []byte(clipBytes)))
	must(t, err)
	calls := second.received()
	if out.Text != "from the second server" || len(calls) != 1 || calls[0].model != "whisper-1" || len(first.received()) != 1 {
		t.Fatalf("after a reload: %q %+v", out.Text, calls)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Effective.Transcribe.URL != second.url() || cfg.Effective.Transcribe.Model != "whisper-1" {
		t.Fatalf("effective %+v", cfg.Effective.Transcribe)
	}

	// Emptying the url turns transcription off again.
	files.write(t, strings.Replace(files.topText, first.url(), "", 1), files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	_, err = c.Transcribe(ctx, clipRequest("audio/webm", []byte(clipBytes)))
	transcribeError(t, err, TranscriptionOff)
	if len(second.received()) != 1 {
		t.Fatal("the second server was contacted while transcription was off")
	}
}

// A server that fails in any way yields a transcription error and no text.
func TestTranscribeReportsServerFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		tune    func(*Options)
		want    string
	}{
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"text":"`+whisperText+`"}`, http.StatusInternalServerError)
		}, nil, "status 500"},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }, nil, "status 401"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
		}, nil, "status 302"},
		{"not json", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "plain "+whisperText) }, nil, "not a transcription"},
		{"no text", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"words":[]}`) }, nil, "not a transcription"},
		{"text of the wrong type", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"text":7}`) }, nil, "not a transcription"},
		{"oversized reply", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"text":"`+strings.Repeat("a", maxTranscribeReply)+`"}`)
		}, nil, "not a transcription"},
		{"slow server", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, func(o *Options) { o.transcribeTimeout = 100 * time.Millisecond }, "did not answer in time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			whisper, _, c, _, log := transcribeFixture(t, map[string]string{"WHISPER_API_KEY": whisperKey}, tc.tune)
			whisper.handler = tc.handler
			out, err := c.Transcribe(context.Background(), clipRequest("audio/webm", []byte(clipBytes)))
			api := transcribeError(t, err, TranscriptionFailed)
			if out.Text != "" || !strings.Contains(api.Message, tc.want) {
				t.Fatalf("%q %+v", out.Text, api)
			}
			for _, secret := range []string{whisperKey, whisperText, clipBytes, base64.StdEncoding.EncodeToString([]byte(clipBytes))} {
				if strings.Contains(api.Message, secret) || strings.Contains(log.String(), secret) {
					t.Fatalf("%q leaked into the error or the log: %+v %s", secret, api, log.String())
				}
			}
		})
	}

	t.Run("unreachable server", func(t *testing.T) {
		t.Parallel()
		whisper, _, c, _, log := transcribeFixture(t, map[string]string{"WHISPER_API_KEY": whisperKey}, nil)
		whisper.Close()
		out, err := c.Transcribe(context.Background(), clipRequest("audio/webm", []byte(clipBytes)))
		api := transcribeError(t, err, TranscriptionFailed)
		if out.Text != "" || !strings.Contains(api.Message, "could not be reached") {
			t.Fatalf("%q %+v", out.Text, api)
		}
		if strings.Contains(log.String(), whisperKey) {
			t.Fatalf("the key reached the log: %s", log.String())
		}
	})
}

// A recording over the limit, an empty one, one that is not base64 and one
// that is not audio are refused before the server is contacted.
func TestTranscribeRefusesBadUploadsWithoutForwarding(t *testing.T) {
	t.Parallel()
	whisper, _, c, _, log := transcribeFixture(t, nil, func(o *Options) { o.transcribeLimit = 1024 })
	ctx := context.Background()
	big := bytes.Repeat([]byte("x"), 1025)
	for _, tc := range []struct {
		name string
		req  TranscribeRequest
		want Code
	}{
		{"over the limit", clipRequest("audio/webm", big), TooLarge},
		{"far over the limit", clipRequest("audio/webm", bytes.Repeat([]byte("x"), 64<<10)), TooLarge},
		{"empty", clipRequest("audio/webm", nil), Validation},
		{"not base64", TranscribeRequest{MIMEType: "audio/webm", Audio: "not*base64!" + clipBytes}, Malformed},
		{"base64 without padding", TranscribeRequest{MIMEType: "audio/webm", Audio: "QUJD" + "RA"}, Malformed},
		{"not audio", clipRequest("text/plain", []byte(clipBytes)), Validation},
		{"video", clipRequest("video/webm", []byte(clipBytes)), Validation},
		{"no type", clipRequest("", []byte(clipBytes)), Validation},
		{"bad type", clipRequest("audio/we bm", []byte(clipBytes)), Validation},
		{"header injection", clipRequest("audio/webm\r\nX-Evil: 1", []byte(clipBytes)), Validation},
	} {
		_, err := c.Transcribe(ctx, tc.req)
		api := transcribeError(t, err, tc.want)
		if strings.Contains(api.Message, clipBytes) || tc.req.Audio != "" && strings.Contains(api.Message, tc.req.Audio) {
			t.Fatalf("%s: the error echoes the upload: %s", tc.name, api.Message)
		}
	}
	// The limit is on the clip, not on its encoding.
	_, err := c.Transcribe(ctx, clipRequest("audio/webm", big[:1024]))
	must(t, err)
	if calls := whisper.received(); len(calls) != 1 || len(calls[0].file) != 1024 {
		t.Fatalf("the server received %d requests", len(calls))
	}
	if strings.Contains(log.String(), "xxxxxxxx") {
		t.Fatal("the upload reached the log")
	}
}

// The default limit refuses a clip over 25 MiB, and a body that is not one
// JSON object with known fields is malformed without being echoed.
func TestTranscribeDefaultLimitAndMalformedBodies(t *testing.T) {
	t.Parallel()
	whisper, s, _, _, log := transcribeFixture(t, nil, nil)
	web := socketHTTP(s.Socket())
	post := func(body string) (int, string) {
		code, out := exchange(t, web, "POST", "http://osmia"+Prefix+"/transcribe", "application/json", body, "")
		return code, string(out)
	}
	huge := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, maxTranscribeClip+1))
	code, body := post(`{"mime_type":"audio/webm","audio":"` + huge + `"}`)
	if code != http.StatusRequestEntityTooLarge || errorCode(t, []byte(body)) != TooLarge {
		t.Fatalf("a clip over 25 MiB: %d %.200s", code, body)
	}
	for _, malformed := range []string{
		``, `[]`, `"audio"`, `{"mime_type":"audio/webm","audio":"QUJD","extra":1}`,
		`{"mime_type":"audio/webm","audio":"QUJD","audio":"QUJD"}`, `{"mime_type":"audio/webm","audio":7}`,
		`{"mime_type":"audio/webm","audio":"QUJD"} {}`, `{"mime_type":"audio/webm","audio":"` + clipBytes + `"`,
	} {
		code, body = post(malformed)
		if code != http.StatusBadRequest || errorCode(t, []byte(body)) != Malformed || strings.Contains(body, clipBytes) || strings.Contains(body, "QUJD") {
			t.Fatalf("body %.60q: %d %s", malformed, code, body)
		}
	}
	if calls := whisper.received(); len(calls) != 0 {
		t.Fatalf("the server received %d requests", len(calls))
	}
	if strings.Contains(log.String(), clipBytes) || strings.Contains(log.String(), "QUJD") {
		t.Fatal("the upload reached the log")
	}
}

// The web listener keeps its protections for the route: a Host that is not
// loopback and a write that is not JSON are forbidden, and the Whisper
// server receives nothing for them.
func TestTranscribeKeepsTheWebListenerProtections(t *testing.T) {
	t.Parallel()
	whisper := newFakeWhisper(t)
	opts := fixture(t)
	withTranscribe(t, opts, "url = \""+whisper.url()+"\"\n")
	withWeb(t, opts, "127.0.0.1:0")
	s, _ := start(t, opts)
	web, base := webHTTP(), "http://"+s.WebAddr()+Prefix+"/transcribe"
	valid, err := json.Marshal(clipRequest("audio/webm", []byte(clipBytes)))
	must(t, err)

	for _, tc := range []struct{ name, contentType, body, host string }{
		{"foreign host", "application/json", string(valid), "osmia.example.com"},
		{"foreign host with port", "application/json", string(valid), "attacker.example:8484"},
		{"rebound address", "application/json", string(valid), "192.168.1.5:8484"},
		{"raw audio", "audio/webm", clipBytes, ""},
		{"multipart", "multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.webm\"\r\nContent-Type: audio/webm\r\n\r\n" + clipBytes + "\r\n--x--\r\n", ""},
		{"plain text", "text/plain", string(valid), ""},
		{"form encoding", "application/x-www-form-urlencoded", "audio=" + clipBytes, ""},
		{"no content type", "", string(valid), ""},
	} {
		code, body := exchange(t, web, "POST", base, tc.contentType, tc.body, tc.host)
		if code != http.StatusForbidden || errorCode(t, body) != Forbidden {
			t.Fatalf("%s: %d %s", tc.name, code, body)
		}
	}
	if calls := whisper.received(); len(calls) != 0 {
		t.Fatalf("the server received %+v", calls)
	}

	// A JSON request from the loopback address is served.
	code, body := exchange(t, web, "POST", base, "application/json; charset=utf-8", string(valid), "")
	var out TranscribeResponse
	must(t, json.Unmarshal(body, &out))
	if code != 200 || out.Text != whisperText || len(whisper.received()) != 1 {
		t.Fatalf("a JSON request over the web listener: %d %s", code, body)
	}
}

// Neither the key, the clip nor the text reaches /v1/config, the log or a
// file under the root.
func TestTranscribeKeepsSecretsAndContentOutOfRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	whisper, s, c, opts, log := transcribeFixture(t, map[string]string{"WHISPER_API_KEY": whisperKey}, nil)
	clip := []byte(strings.Repeat(clipBytes, 40))
	encoded := base64.StdEncoding.EncodeToString(clip)
	for range 2 {
		out, err := c.Transcribe(ctx, clipRequest("audio/webm;codecs=opus", clip))
		must(t, err)
		if out.Text != whisperText {
			t.Fatalf("text %q", out.Text)
		}
	}
	whisper.handler = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom "+whisperKey, 500) }
	_, err := c.Transcribe(ctx, clipRequest("audio/webm", clip))
	transcribeError(t, err, TranscriptionFailed)

	cfg, err := c.Configuration(ctx)
	must(t, err)
	config, err := json.Marshal(cfg)
	must(t, err)
	status, err := c.Statuses(ctx)
	must(t, err)
	statusJSON, err := json.Marshal(status)
	must(t, err)
	if strings.Contains(string(config)+string(statusJSON), whisperKey) {
		t.Fatal("the key is in the configuration or status response")
	}
	forbidden := []string{whisperKey, whisperText, clipBytes, encoded[:64]}
	for _, secret := range forbidden {
		if strings.Contains(log.String(), secret) {
			t.Fatalf("%q reached the log: %s", secret, log.String())
		}
	}
	must(t, s.Close())
	must(t, filepath.WalkDir(filepath.Dir(opts.Config.Root), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, secret := range forbidden {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%q is in %s", secret, path)
			}
		}
		return nil
	}))
}

// The route is in Routes with a JSON request body.
func TestTranscribeRouteIsListed(t *testing.T) {
	t.Parallel()
	for _, route := range Routes {
		if route.Method == http.MethodPost && route.Path == Prefix+"/transcribe" {
			if _, ok := route.Request.(TranscribeRequest); !ok {
				t.Fatalf("request %T", route.Request)
			}
			if _, ok := route.Response.(TranscribeResponse); !ok {
				t.Fatalf("response %T", route.Response)
			}
			return
		}
	}
	t.Fatal("POST /v1/transcribe is not in Routes")
}
