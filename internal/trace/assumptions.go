package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Assumption is one judgment call the chief of staff made within the
// workstream's stated intent, logged rather than asked. Its text appears in
// the workstream's drafted pull request description.
type Assumption struct {
	Workstreams []config.WorkstreamID `json:"workstreams,omitempty"`
	ID          string                `json:"id"`
	Text        string                `json:"text"`
	Actor       Actor                 `json:"actor"`
	Thread      string                `json:"thread"`
	Turn        string                `json:"turn"`
	At          time.Time             `json:"at"`
}

// AssumptionsPath is the workstream document holding its logged assumptions,
// and AssumptionsDocument its record ID.
const (
	AssumptionsPath     = "assumptions.json"
	AssumptionsDocument = "assumptions"
)

type assumptionsFile struct {
	Version     int          `json:"version"`
	Assumptions []Assumption `json:"assumptions"`
}

// Assumptions returns local judgments followed by the shared chief's judgments
// affecting stream. Shared judgments stay authoritative on the chief's host;
// reading them never requires another member's history.
func Assumptions(r *Repository, stream config.WorkstreamID) ([]Assumption, error) {
	local, err := readAssumptions(r, stream)
	if err != nil {
		return nil, err
	}
	project := r
	if r.parent != nil {
		project = r.parent
	}
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return nil, err
	}
	if id != "" && project.Project() != crossRepoProject(id) {
		authority, err := OpenWorkgroupAuthority(r.root, id)
		if err != nil {
			if authority != nil {
				authority.Close()
			}
			return nil, err
		}
		defer authority.Close()
		project = authority
	}
	groups, _, err := Workgroups(project)
	if err != nil {
		return nil, err
	}
	if id != "" {
		g, found := WorkgroupOf(groups, stream)
		if !found || g.ID != id {
			return nil, fmt.Errorf("workstream %s is not recorded in workgroup %s", stream, id)
		}
	}
	host := chiefOfStaffHost(groups, stream)
	result := applicableAssumptions(local, stream)
	if host == stream {
		return result, nil
	}
	shared, err := readAssumptions(project, host)
	if err != nil {
		return nil, err
	}
	return append(result, applicableAssumptions(shared, stream)...), nil
}

func applicableAssumptions(all []Assumption, stream config.WorkstreamID) []Assumption {
	var result []Assumption
	for _, a := range all {
		if len(a.Workstreams) == 0 || slices.Contains(a.Workstreams, stream) {
			result = append(result, a)
		}
	}
	return result
}

func readAssumptions(r *Repository, stream config.WorkstreamID) ([]Assumption, error) {
	records, err := Read[Document](r, stream)
	if err != nil {
		return nil, err
	}
	content, revision := "", 0
	for _, d := range records {
		if d.Path == AssumptionsPath && d.Revision > revision {
			content, revision = d.Content, d.Revision
		}
	}
	if content == "" {
		return nil, nil
	}
	var file assumptionsFile
	if err := json.Unmarshal([]byte(content), &file); err != nil {
		return nil, err
	}
	return file.Assumptions, nil
}

// LogAssumption records text as the next logged assumption of the scope's
// active turn's workstream, in one commit. A chief can select affected members;
// omission applies to the whole workgroup. Workers can only select themselves.
// Replaying the same text and targets in a turn returns the original entry.
func (r *Repository) LogAssumption(ctx context.Context, agent string, scope coreadapter.Scope, text string, at time.Time, affected ...config.WorkstreamID) (Assumption, error) {
	stream := config.WorkstreamID(scope.Workstream)
	// Resolve membership before acquiring the scoped mutex: group documents and
	// the active turn live in different scopes.
	members := []config.WorkstreamID{stream}
	if scope.Role == ChiefOfStaff {
		var err error
		project := r
		if r.parent != nil {
			project = r.parent
		}
		members, err = project.WorkgroupMembers(stream)
		if err != nil {
			return Assumption{}, err
		}
	}
	targets := slices.Clone(affected)
	if len(targets) == 0 {
		targets = slices.Clone(members)
	}
	slices.Sort(targets)
	for i, target := range targets {
		if !slices.Contains(members, target) {
			return Assumption{}, refused("workstream %s does not share this chief of staff", target)
		}
		if i > 0 && targets[i-1] == target {
			return Assumption{}, refused("workstream %s is listed twice", target)
		}
	}
	r, err := r.scoped(stream)
	if err != nil {
		return Assumption{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, _, _, records, err := r.questionTurn(ctx, agent, scope, at)
	if err != nil {
		return Assumption{}, err
	}
	if !present(text) {
		return Assumption{}, refused("text is required: the judgment call, as it should read in the drafted pull request")
	}
	content, revision := "", 0
	for _, rec := range records {
		if d, ok := rec.(Document); ok && d.Workstream == stream && d.Path == AssumptionsPath && d.Revision > revision {
			content, revision = d.Content, d.Revision
		}
	}
	var file assumptionsFile
	if content != "" {
		if err := json.Unmarshal([]byte(content), &file); err != nil {
			return Assumption{}, err
		}
	}
	for _, a := range file.Assumptions {
		if a.Thread == scope.Thread && a.Turn == scope.Turn && a.Text == text && (slices.Equal(a.Workstreams, targets) || len(a.Workstreams) == 0 && len(affected) == 0) {
			return a, nil
		}
	}
	a := Assumption{Workstreams: targets, ID: strconv.Itoa(len(file.Assumptions) + 1), Text: text, Actor: Actor{Kind: "agent", ID: agent}, Thread: scope.Thread, Turn: scope.Turn, At: at}
	file.Version = 1
	file.Assumptions = append(file.Assumptions, a)
	data, err := json.Marshal(file)
	if err != nil {
		return Assumption{}, err
	}
	doc := Document{
		Header:  Header{Schema: "osmia.trace.document", Version: Version, ID: AssumptionsDocument, Revision: revision + 1, Project: r.project, Workstream: stream, At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1},
		Path:    AssumptionsPath,
		Content: string(data),
	}
	files, err := r.documentFiles(ctx, []Document{doc})
	if err != nil {
		return Assumption{}, err
	}
	if err := r.publish(ctx, files); err != nil {
		return Assumption{}, err
	}
	return a, nil
}
