package filetransport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

// transferTimeout bounds one archive upload or download and one git exchange,
// which outlast the service's ordinary request deadlines.
const transferTimeout = 30 * time.Minute

// Sink takes the unpacked view uploaded for a turn. The directory staging is
// removed when the sink returns, so a sink keeps what it wants by copying it
// out before then. The answer is what the uploading client is told.
type Sink func(ctx context.Context, turn, staging string) (api.ViewAnswer, error)

// Turn is what the service holds for a turn that moves a view.
type Turn struct {
	// Token is the bearer token the turn's requests carry, the one its tool
	// endpoint requires.
	Token string
	// TokenSum, instead of Token, is the hex SHA-256 of that token, for a turn
	// the service no longer holds the token of, such as one that survived a
	// restart.
	TokenSum string
	// Download is a directory the turn's view archive is made from; empty for a
	// turn whose view the client builds itself.
	Download string
	// Upload receives the finished view; nil for a turn that takes none.
	Upload Sink
}

// Registry holds the turns that may move views and unpacks what they upload.
type Registry struct {
	staging string
	limits  Limits
	mu      sync.Mutex
	turns   map[string]registered
	next    uint64
	lookup  func(turn string) (Turn, bool)
}

type registered struct {
	Turn
	id uint64
}

// NewRegistry returns a registry that unpacks uploads into fresh directories
// under staging, which it creates when needed.
func NewRegistry(staging string) *Registry {
	return &Registry{staging: staging, limits: DefaultLimits, turns: map[string]registered{}}
}

// Register makes turn's view routes answer to t's token and returns the
// function that ends it. Registering a turn again replaces the earlier
// registration, and the earlier release then leaves the new one alone.
func (g *Registry) Register(turn string, t Turn) (release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	id := g.next
	g.turns[turn] = registered{Turn: t, id: id}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.turns[turn].id == id {
			delete(g.turns, turn)
		}
	}
}

// SetLookup gives the registry a way to find a turn it holds no registration
// for, such as a turn that survived a service restart. The turn it returns is
// authorised by its TokenSum.
func (g *Registry) SetLookup(f func(turn string) (Turn, bool)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lookup = f
}

func (g *Registry) authorise(r *http.Request, turn string) (Turn, *api.APIError) {
	g.mu.Lock()
	t, ok := g.turns[turn]
	lookup := g.lookup
	g.mu.Unlock()
	if !ok && lookup != nil {
		var found Turn
		if found, ok = lookup(turn); ok {
			t = registered{Turn: found}
		}
	}
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !bearer || !t.accepts(token) {
		return Turn{}, &api.APIError{Code: api.Forbidden, Message: "the request does not carry the turn's bearer token"}
	}
	return t.Turn, nil
}

func (t Turn) accepts(token string) bool {
	switch {
	case t.Token != "":
		return sameToken(token, t.Token)
	case t.TokenSum != "":
		sum := sha256.Sum256([]byte(token))
		return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(t.TokenSum)) == 1
	}
	return false
}

// TokenSum is the hex SHA-256 of a turn's bearer token.
func TokenSum(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sameToken(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func extendDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(transferTimeout)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

// Download answers a GET with the archive of the turn's view directory. It
// returns the refusal to send, or nil once it has answered.
func (g *Registry) Download(w http.ResponseWriter, r *http.Request, turn string) *api.APIError {
	t, refusal := g.authorise(r, turn)
	if refusal != nil {
		return refusal
	}
	if t.Download == "" {
		return &api.APIError{Code: api.NotFound, Message: "the turn has no view archive to download"}
	}
	extendDeadlines(w)
	w.Header().Set("Content-Type", api.ViewArchiveType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := Pack(w, t.Download); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

// Upload unpacks the archive in a PUT body into a staging directory and hands
// it to the turn's sink. It returns the refusal to send, or the answer to
// send as JSON.
func (g *Registry) Upload(w http.ResponseWriter, r *http.Request, turn string) (api.ViewUploadResponse, *api.APIError) {
	t, refusal := g.authorise(r, turn)
	if refusal != nil {
		return api.ViewUploadResponse{}, refusal
	}
	if t.Upload == nil {
		return api.ViewUploadResponse{}, &api.APIError{Code: api.NotFound, Message: "the turn takes no view"}
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != api.ViewArchiveType {
		return api.ViewUploadResponse{}, &api.APIError{Code: api.Malformed, Message: "expected Content-Type " + api.ViewArchiveType}
	}
	extendDeadlines(w)
	if err := os.MkdirAll(g.staging, 0o700); err != nil {
		return api.ViewUploadResponse{}, &api.APIError{Code: api.Unavailable, Message: "the view cannot be staged"}
	}
	staging, err := os.MkdirTemp(g.staging, "view-")
	if err != nil {
		return api.ViewUploadResponse{}, &api.APIError{Code: api.Unavailable, Message: "the view cannot be staged"}
	}
	defer os.RemoveAll(staging)
	body := http.MaxBytesReader(w, r.Body, g.limits.Bytes)
	if err := unpack(body, staging, g.limits); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return api.ViewUploadResponse{}, &api.APIError{Code: api.TooLarge, Message: "the view archive is too large"}
		}
		return api.ViewUploadResponse{}, &api.APIError{Code: api.Malformed, Message: "the view archive is refused: " + err.Error()}
	}
	answer, err := t.Upload(r.Context(), turn, staging)
	if err != nil {
		return api.ViewUploadResponse{}, &api.APIError{Code: api.Unavailable, Message: "the view could not be taken: " + err.Error()}
	}
	return api.ViewUploadResponse{Answer: answer}, nil
}
