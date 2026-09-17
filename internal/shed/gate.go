package shed

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Redraft is the owner's request, recorded against the round debate concluded
// at, for the architect to redraft the spec and the plan, with the note that
// says what to change.
type Redraft struct {
	Version  int    `json:"version"`
	Round    int    `json:"round"`
	Revision Pin    `json:"revision"`
	Note     string `json:"note"`
}

// RedraftPath is the workstream path of the owner's request for a redraft
// after round n, and RedraftedPath of the architect's report of the redraft it
// wrote for that request.
func RedraftPath(round int) string   { return roundPath(round, redraftName) }
func RedraftedPath(round int) string { return roundPath(round, redraftedName) }

// RedraftDocumentID is the trace record ID of the owner's request, and
// RedraftedDocumentID of the architect's report of the redraft.
func RedraftDocumentID(round int) string   { return roundDocumentID(round, redraftName) }
func RedraftedDocumentID(round int) string { return roundDocumentID(round, redraftedName) }

func (r Redraft) check() error {
	switch {
	case r.Version != Version:
		return fmt.Errorf("unsupported shed redraft version %d", r.Version)
	case r.Round < 1:
		return errors.New("a request for a redraft requires the round debate concluded at")
	case r.Revision.Spec < 1 || r.Revision.Plan < 1:
		return errors.New("a request for a redraft requires the revision it was made against")
	case strings.TrimSpace(r.Note) == "":
		return errors.New("a request for a redraft requires the owner's note")
	}
	return nil
}

// EncodeRedraft returns the file content of a valid request for a redraft.
func EncodeRedraft(r Redraft) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return encodeFile(r)
}

// ParseRedraft reads the file content of a request for a redraft, refusing
// unknown fields and invalid requests.
func ParseRedraft(data []byte) (Redraft, error) {
	var r Redraft
	if err := decodeFile(data, &r, "shed redraft"); err != nil {
		return Redraft{}, err
	}
	return r, r.check()
}

// Redrafts returns the owner's requests for a redraft, ordered by the round
// they were made after. A request whose content disagrees with its path is an
// error.
func Redrafts(repository *trace.Repository, stream config.WorkstreamID) ([]Redraft, error) {
	latest, err := shedFiles(repository, stream, redraftName)
	if err != nil {
		return nil, err
	}
	var out []Redraft
	for path, d := range latest {
		r, err := ParseRedraft([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if RedraftPath(r.Round) != path {
			return nil, fmt.Errorf("%s: records the request made after round %d", path, r.Round)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Redraft) int { return a.Round - b.Round })
	return out, nil
}

// Packet is what the chief of staff presents at the owner's decision point:
// the revisions of the spec and the plan the owner decides on, why debate
// ended, the dissent record with what blocks ratification first, and the
// recommendation. Skipped says the owner skipped debate rather than the
// committee concluding it.
type Packet struct {
	Version        int     `json:"version"`
	Round          int     `json:"round"`
	Revision       Pin     `json:"revision"`
	Skipped        bool    `json:"skipped"`
	Conclusion     string  `json:"conclusion"`
	Dissent        []Entry `json:"dissent"`
	Recommendation string  `json:"recommendation"`
}

// PacketPath is the workstream path of the ratification packet of a round,
// and PacketDocumentID its trace record ID.
func PacketPath(round int) string       { return roundPath(round, packetName) }
func PacketDocumentID(round int) string { return roundDocumentID(round, packetName) }

func (p Packet) check() error {
	switch {
	case p.Version != Version:
		return fmt.Errorf("unsupported ratification packet version %d", p.Version)
	case p.Round < 1:
		return errors.New("a ratification packet requires the round it is presented after")
	case p.Revision.Spec < 1 || p.Revision.Plan < 1:
		return errors.New("a ratification packet requires the revisions it presents")
	case strings.TrimSpace(p.Conclusion) == "":
		return errors.New("a ratification packet requires why debate ended")
	case strings.TrimSpace(p.Recommendation) == "":
		return errors.New("a ratification packet requires a recommendation")
	}
	return nil
}

// Present builds the packet for a decision point: the entries that block
// ratification come first, each one marked blocking or advisory by the dissent
// record it comes from, and the recommendation follows from what blocks.
func Present(round int, revision Pin, skipped bool, conclusion string, entries []Entry) Packet {
	dissent := slices.Clone(entries)
	slices.SortStableFunc(dissent, func(a, b Entry) int {
		if a.Blocking == b.Blocking {
			return 0
		}
		if a.Blocking {
			return -1
		}
		return 1
	})
	return Packet{Version: Version, Round: round, Revision: revision, Skipped: skipped, Conclusion: conclusion,
		Dissent: dissent, Recommendation: Recommend(entries)}
}

// Recommend is what the chief of staff advises the owner to do about the
// dissent that stands: ratify when nothing blocks, and otherwise dispose of
// what blocks or send the draft back to the architect.
func Recommend(entries []Entry) string {
	blocked := Blocked(entries)
	switch {
	case len(blocked) > 0:
		ids := make([]string, len(blocked))
		for i, e := range blocked {
			ids[i] = e.ID
		}
		return fmt.Sprintf("do not ratify yet: %s block ratification (%s); overrule or sustain each one, or ask for a redraft",
			Objections(len(blocked)), strings.Join(ids, ", "))
	case len(entries) > 0:
		return fmt.Sprintf("ratify: nothing blocks, and %s stand as advice on the record", Objections(len(entries)))
	}
	return "ratify: no objection stands"
}

// Objections counts objections for a message.
func Objections(n int) string {
	if n == 1 {
		return "1 objection"
	}
	return strconv.Itoa(n) + " objections"
}

// PacketRound returns the round whose ratification packet a workstream path
// holds, and whether it is one.
func PacketRound(path string) (int, bool) {
	rest, ok := strings.CutPrefix(path, "shed/round-")
	if !ok {
		return 0, false
	}
	number, ok := strings.CutSuffix(rest, "/"+packetName+".json")
	if !ok {
		return 0, false
	}
	round, err := strconv.Atoi(number)
	return round, err == nil && round > 0
}

// EncodePacket returns the file content of a valid packet.
func EncodePacket(p Packet) ([]byte, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if p.Dissent == nil {
		p.Dissent = []Entry{}
	}
	return encodeFile(p)
}

// ParsePacket reads a packet's file content, refusing unknown fields and
// invalid packets.
func ParsePacket(data []byte) (Packet, error) {
	var p Packet
	if err := decodeFile(data, &p, "ratification packet"); err != nil {
		return Packet{}, err
	}
	if len(p.Dissent) == 0 {
		p.Dissent = nil
	}
	return p, p.check()
}

// Packets returns the workstream's recorded ratification packets, ordered by
// the round they were presented after. A packet whose content disagrees with
// its path is an error.
func Packets(repository *trace.Repository, stream config.WorkstreamID) ([]Packet, error) {
	latest, err := shedFiles(repository, stream, packetName)
	if err != nil {
		return nil, err
	}
	var out []Packet
	for path, d := range latest {
		p, err := ParsePacket([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if PacketPath(p.Round) != path {
			return nil, fmt.Errorf("%s: records the packet of round %d", path, p.Round)
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Packet) int { return a.Round - b.Round })
	return out, nil
}

// Ratification is the owner's approval of one revision of the spec and one of
// the plan, with the dispositions of the dissent that stood when they were
// approved.
type Ratification struct {
	Version      int      `json:"version"`
	Round        int      `json:"round"`
	Revision     Pin      `json:"revision"`
	Dispositions []Ruling `json:"dispositions"`
	Dissent      []Entry  `json:"dissent"`
}

// RatificationPath is the workstream path of the owner's ratification,
// recorded under the round it was given in, and RatificationDocumentID its
// trace record ID.
func RatificationPath(round int) string       { return roundPath(round, ratificationName) }
func RatificationDocumentID(round int) string { return roundDocumentID(round, ratificationName) }

func (r Ratification) check() error {
	switch {
	case r.Version != Version:
		return fmt.Errorf("unsupported ratification version %d", r.Version)
	case r.Round < 1:
		return errors.New("a ratification requires the round it was given in")
	case r.Revision.Spec < 1 || r.Revision.Plan < 1:
		return errors.New("a ratification requires the revisions it approves")
	}
	for _, e := range r.Dissent {
		if e.Blocking {
			return fmt.Errorf("objection %s blocks and cannot be ratified", e.ID)
		}
	}
	seen := map[string]bool{}
	for _, one := range r.Dispositions {
		switch {
		case strings.TrimSpace(one.Objection) == "":
			return errors.New("a disposition requires the objection it disposes of")
		case !slices.Contains(Dispositions, one.Disposition):
			return fmt.Errorf("unsupported disposition %q", one.Disposition)
		case seen[one.Objection]:
			return fmt.Errorf("objection %s is disposed of twice", one.Objection)
		}
		seen[one.Objection] = true
	}
	return nil
}

// Ratify returns the record of the owner's approval of the given revisions,
// with the dissent record it was given over and the dispositions in force.
func Ratify(round int, revision Pin, entries []Entry) Ratification {
	r := Ratification{Version: Version, Round: round, Revision: revision, Dissent: slices.Clone(entries)}
	for _, e := range entries {
		if e.Disposition != "" {
			r.Dispositions = append(r.Dispositions, Ruling{Objection: e.ID, Disposition: e.Disposition, Note: e.Note})
		}
	}
	return r
}

// EncodeRatification returns the file content of a valid ratification.
func EncodeRatification(r Ratification) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if r.Dispositions == nil {
		r.Dispositions = []Ruling{}
	}
	if r.Dissent == nil {
		r.Dissent = []Entry{}
	}
	return encodeFile(r)
}

// ParseRatification reads a ratification's file content, refusing unknown
// fields and invalid ratifications.
func ParseRatification(data []byte) (Ratification, error) {
	var r Ratification
	if err := decodeFile(data, &r, "ratification"); err != nil {
		return Ratification{}, err
	}
	if len(r.Dispositions) == 0 {
		r.Dispositions = nil
	}
	if len(r.Dissent) == 0 {
		r.Dissent = nil
	}
	return r, r.check()
}

// Ratifications returns the owner's recorded ratifications of the workstream,
// ordered by the round they were given in. A ratification whose content
// disagrees with its path is an error.
func Ratifications(repository *trace.Repository, stream config.WorkstreamID) ([]Ratification, error) {
	latest, err := shedFiles(repository, stream, ratificationName)
	if err != nil {
		return nil, err
	}
	var out []Ratification
	for path, d := range latest {
		r, err := ParseRatification([]byte(d.Content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if RatificationPath(r.Round) != path {
			return nil, fmt.Errorf("%s: records the ratification of round %d", path, r.Round)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Ratification) int { return a.Round - b.Round })
	return out, nil
}

func roundPath(round int, name string) string {
	return "shed/round-" + strconv.Itoa(round) + "/" + name + ".json"
}

func roundDocumentID(round int, name string) string {
	return "shed-round-" + strconv.Itoa(round) + "-" + name
}
