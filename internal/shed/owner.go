package shed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// OwnerMember is the member name of the owner's own objections. No
	// committee member may carry it.
	OwnerMember = "owner"
	// rulingsName is the file name, without its extension, of the owner's
	// rulings of a round.
	rulingsName = "rulings"
	// moreName is the file name, without its extension, of the owner's
	// request for further rounds after a round concluded the debate.
	moreName = "more"
)

// Owner is the kind of the owner's own objections. It is not one of Kinds: a
// committee member cannot raise it, and unlike a member's objection it needs
// neither a part nor a citation. Like every kind but Fit it blocks.
const Owner Kind = "owner"

// reserved reports whether a shed file name, without its extension, belongs
// to the architect or to the owner rather than to a committee member.
func reserved(name string) bool {
	return name == replyName || name == rulingsName || name == moreName
}

// reservedPath reports whether a shed path names one of the reserved files.
func reservedPath(path string) bool {
	name := path[strings.LastIndex(path, "/")+1:]
	return strings.HasSuffix(name, ".json") && reserved(strings.TrimSuffix(name, ".json"))
}

// Disposition is what the owner ruled about an objection.
type Disposition string

const (
	// Sustained says the objection stands and blocks until it is conceded.
	Sustained Disposition = "sustained"
	// Dismissed says the objection is settled by the owner: it is kept as a
	// recorded disposition and no longer blocks or holds up the debate.
	Dismissed Disposition = "dismissed"
)

// Dispositions lists the rulings the owner may make.
var Dispositions = []Disposition{Sustained, Dismissed}

// ParseDisposition returns the disposition the owner asked for, written as
// the verb the CLI and the API take.
func ParseDisposition(s string) (Disposition, bool) {
	switch s {
	case "sustain":
		return Sustained, true
	case "dismiss":
		return Dismissed, true
	}
	return "", false
}

// Ruling is the owner's disposition of one objection, with the owner's note.
type Ruling struct {
	Objection   string      `json:"objection"`
	Disposition Disposition `json:"disposition"`
	Note        string      `json:"note,omitempty"`
}

// Rulings is what the owner ruled during one round, against the revision the
// documents were at.
type Rulings struct {
	Version  int      `json:"version"`
	Round    int      `json:"round"`
	Revision Pin      `json:"revision"`
	Rulings  []Ruling `json:"rulings"`
}

// RulingsPath is the workstream path of the owner's rulings of a round.
func RulingsPath(round int) string {
	return "shed/round-" + strconv.Itoa(round) + "/" + rulingsName + ".json"
}

// RulingsDocumentID is the trace record ID of the owner's rulings of a round.
func RulingsDocumentID(round int) string {
	return "shed-round-" + strconv.Itoa(round) + "-" + rulingsName
}

func (r Rulings) check() error {
	switch {
	case r.Version != Version:
		return fmt.Errorf("unsupported shed rulings version %d", r.Version)
	case r.Round < 1:
		return errors.New("shed rulings require a positive round")
	case r.Revision.Spec < 1 || r.Revision.Plan < 1:
		return errors.New("shed rulings require the revision they were made against")
	}
	seen := map[string]bool{}
	for _, one := range r.Rulings {
		switch {
		case strings.TrimSpace(one.Objection) == "":
			return errors.New("a ruling requires the objection it rules on")
		case !slices.Contains(Dispositions, one.Disposition):
			return fmt.Errorf("unsupported disposition %q", one.Disposition)
		case seen[one.Objection]:
			return fmt.Errorf("objection %s is ruled on twice", one.Objection)
		}
		seen[one.Objection] = true
	}
	return nil
}

// EncodeRulings returns the file content of valid rulings.
func EncodeRulings(r Rulings) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if r.Rulings == nil {
		r.Rulings = []Ruling{}
	}
	return encodeFile(r)
}

// ParseRulings reads a rulings file's content, refusing unknown fields and
// invalid rulings.
func ParseRulings(data []byte) (Rulings, error) {
	var r Rulings
	if err := decodeFile(data, &r, "shed rulings"); err != nil {
		return Rulings{}, err
	}
	if len(r.Rulings) == 0 {
		r.Rulings = nil
	}
	return r, r.check()
}

// More is the owner's request, recorded against the round debate concluded
// at, for that many further rounds.
type More struct {
	Version int `json:"version"`
	Round   int `json:"round"`
	Rounds  int `json:"rounds"`
}

// MorePath is the workstream path of the owner's request for further rounds
// after round n.
func MorePath(round int) string {
	return "shed/round-" + strconv.Itoa(round) + "/" + moreName + ".json"
}

// MoreDocumentID is the trace record ID of that request.
func MoreDocumentID(round int) string {
	return "shed-round-" + strconv.Itoa(round) + "-" + moreName
}

func (m More) check() error {
	switch {
	case m.Version != Version:
		return fmt.Errorf("unsupported shed request version %d", m.Version)
	case m.Round < 1:
		return errors.New("a request for further rounds requires the round debate concluded at")
	case m.Rounds < 1:
		return errors.New("a request for further rounds requires at least one round")
	}
	return nil
}

// EncodeMore returns the file content of a valid request for further rounds.
func EncodeMore(m More) ([]byte, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return encodeFile(m)
}

// ParseMore reads the file content of a request for further rounds, refusing
// unknown fields and invalid requests.
func ParseMore(data []byte) (More, error) {
	var m More
	if err := decodeFile(data, &m, "shed request"); err != nil {
		return More{}, err
	}
	return m, m.check()
}

func encodeFile(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodeFile(data []byte, out any, what string) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s must be one object", what)
	}
	return nil
}

// ownerFiles returns the latest revision of every recorded shed file of the
// workstream whose name, without its extension, is the given one.
func ownerFiles(repository *trace.Repository, stream config.WorkstreamID, name string) (map[string]trace.Document, error) {
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	suffix := "/" + name + ".json"
	for _, d := range documents {
		if strings.HasPrefix(d.Path, "shed/") && strings.HasSuffix(d.Path, suffix) {
			latest[d.Path] = d
		}
	}
	return latest, nil
}

// AllRulings returns the owner's rulings of the workstream, ordered by the
// round they were made in. A rulings file whose content disagrees with its
// path is an error.
func AllRulings(repository *trace.Repository, stream config.WorkstreamID) ([]Rulings, error) {
	latest, err := ownerFiles(repository, stream, rulingsName)
	if err != nil {
		return nil, err
	}
	var out []Rulings
	for path, d := range latest {
		r, err := ParseRulings([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if RulingsPath(r.Round) != path {
			return nil, fmt.Errorf("%s: records the rulings of round %d", path, r.Round)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Rulings) int { return a.Round - b.Round })
	return out, nil
}

// Requests returns the owner's requests for further rounds, ordered by the
// round they were made after. A request whose content disagrees with its path
// is an error.
func Requests(repository *trace.Repository, stream config.WorkstreamID) ([]More, error) {
	latest, err := ownerFiles(repository, stream, moreName)
	if err != nil {
		return nil, err
	}
	var out []More
	for path, d := range latest {
		m, err := ParseMore([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if MorePath(m.Round) != path {
			return nil, fmt.Errorf("%s: records the request made after round %d", path, m.Round)
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b More) int { return a.Round - b.Round })
	return out, nil
}

// Limit is how many rounds of debate the workstream may run: the configured
// cap, or the last round the owner asked for beyond it.
func Limit(configured int, requests []More) int {
	limit := configured
	for _, m := range requests {
		if m.Round+m.Rounds > limit {
			limit = m.Round + m.Rounds
		}
	}
	return limit
}

// flatten returns the rulings of every round in order, latest last.
func flatten(rounds []Rulings) []Ruling {
	var out []Ruling
	for _, r := range rounds {
		out = append(out, r.Rulings...)
	}
	return out
}
