package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// publishInterval is the time between reconcile passes, and
	// publishBackoff the time after a pass that could not reach Hearsay or
	// was refused.
	publishInterval = 15 * time.Second
	publishBackoff  = time.Minute
	// descriptionLimit bounds the prose a workstream record carries from
	// what the owner handed in.
	descriptionLimit = 16 << 10
)

// The core states of Hearsay's work contract.
const (
	coreProposed  = "proposed"
	coreStarted   = "started"
	coreBlocked   = "blocked"
	coreCompleted = "completed"
	coreAbandoned = "abandoned"
)

// publishInputs is what one reconcile pass reads from the committed trace:
// the project and every workstream eligible for Hearsay, read once so every
// record source sees the same snapshot. A record source turns it into the
// records it owns.
type publishInputs struct {
	cfg *config.Config
	// owner is the Hearsay owner principal, named as author of the project
	// and of everything the owner decides.
	owner string
	// workSource and ownerSource are the hooks sources: the work source holds
	// the project and its workstreams, and the owner source holds what the
	// owner decided.
	workSource, ownerSource string
	project                 config.ProjectID
	// created is when the project was registered.
	created time.Time
	// documents are the project's document revisions.
	documents []trace.Document
	streams   []*publishStream
}

// publishStream is one eligible workstream with the trace records its
// Hearsay records are built from.
type publishStream struct {
	status      trace.WorkstreamStatus
	documents   []trace.Document
	transitions []trace.Transition
	evidence    lifecycle.MemberEvidence
	// group is the workgroup of more than one member the workstream belongs
	// to, when it belongs to one.
	group *trace.PublishedWorkgroup
	// approved is false while a cross-repo workstream's seal awaits the
	// workgroup's approval, which keeps its seals unposted.
	approved bool
}

func (s *publishStream) id() config.WorkstreamID { return s.status.Workstream }

// publishSource builds the records one kind of artifact contributes to a
// pass. Sources are independent; the reconciler orders their records by tier.
type publishSource func(*publishInputs) []hearsay.Record

// publishSources are the record sources of a pass.
var publishSources = []publishSource{projectRecords, workstreamRecords, charterRecords, sealedSpecRecords, workgroupSpecRecords}

// publishHearsay joins the project runtime's lifetime. It reconciles the
// project's Hearsay records on a cadence while publishing is configured and
// does nothing otherwise. Hearsay outages and refusals back it off and never
// stop local work or the service.
func (s *Service) publishHearsay(ctx context.Context, repo *trace.Repository) error {
	var delay time.Duration
	var reported string
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		cfg := s.current().For(repo.Project())
		if !cfg.HasProject() {
			return nil
		}
		delay = publishInterval
		if !cfg.Hearsay.Publishing() {
			continue
		}
		res, err := s.publishPass(ctx, repo, cfg)
		if ctx.Err() != nil {
			return nil
		}
		if summary := publishSummary(res, err); summary != reported {
			reported = summary
			if summary != "" {
				s.log().Warn("hearsay publishing incomplete", "project", string(repo.Project()), "reason", summary)
			}
		}
		if err != nil || res.Unreachable || len(res.Failures) > 0 {
			delay = publishBackoff
		}
	}
}

func publishSummary(res hearsay.Result, err error) string {
	var parts []string
	if err != nil {
		parts = append(parts, err.Error())
	}
	if res.Unreachable {
		parts = append(parts, "hearsay hooks are unreachable")
	}
	for _, f := range res.Failures {
		parts = append(parts, f.Record+": "+f.Err.Error())
	}
	if len(parts) > 8 {
		parts = append(parts[:8], fmt.Sprintf("and %d more", len(parts)-8))
	}
	return strings.Join(parts, "; ")
}

// publishPass computes every record from the committed trace, posts those
// whose content differs from what was last posted, and records the digest of
// each successful post in the trace. The first pass is the backfill. A
// refused or unreachable post leaves its digest as it was, so a later pass
// posts it again.
func (s *Service) publishPass(ctx context.Context, repo *trace.Repository, cfg *config.Config) (hearsay.Result, error) {
	token := hearsay.PublisherToken(cfg.Hearsay, nil)
	if token == "" {
		return hearsay.Result{}, fmt.Errorf("the publisher token is not set in %s", cfg.Hearsay.Publish.TokenEnv)
	}
	in, err := collectPublishInputs(ctx, repo, cfg)
	if err != nil {
		return hearsay.Result{}, err
	}
	var records []hearsay.Record
	for _, source := range publishSources {
		records = append(records, source(in)...)
	}
	stored, err := repo.HearsayPosted()
	if err != nil {
		return hearsay.Result{}, err
	}
	posted := make(map[string]hearsay.Posted, len(stored))
	for _, p := range stored {
		posted[hearsay.Ref(p.Source, p.Key)] = hearsay.Posted{Digest: p.Digest, UpdatedAt: p.UpdatedAt}
	}
	res := hearsay.Reconcile(ctx, hearsay.Poster{URL: cfg.Hearsay.Publish.HooksURL, Token: token}, records, posted)
	// What was posted is recorded even when the pass was cut short, so a
	// later pass does not post it again.
	var entries []trace.PostedRecord
	for _, e := range res.Entries {
		entries = append(entries, trace.PostedRecord{Source: e.Source, Key: e.Key, Digest: e.Digest, UpdatedAt: e.UpdatedAt})
	}
	if err := repo.RecordHearsayPosted(context.WithoutCancel(ctx), entries, s.now()); err != nil {
		return res, fmt.Errorf("recording posted digests: %w", err)
	}
	return res, nil
}

// collectPublishInputs reads the project's eligible workstreams: not
// sessions, designs or the librarian's own. A workstream whose trace cannot
// be read this pass, as when it is being cleaned, is left for a later one.
func collectPublishInputs(ctx context.Context, repo *trace.Repository, cfg *config.Config) (*publishInputs, error) {
	publish := cfg.Hearsay.Publish
	in := &publishInputs{cfg: cfg, owner: cfg.Hearsay.Principal, workSource: publish.WorkSource, ownerSource: publish.OwnerSource, project: repo.Project()}
	var err error
	if in.documents, err = trace.Read[trace.Document](repo, ""); err != nil {
		return nil, err
	}
	for _, d := range in.documents {
		if in.created.IsZero() || d.At.Before(in.created) {
			in.created = d.At
		}
	}
	if in.created.IsZero() {
		return nil, errors.New("the project has no recorded history yet")
	}
	eligible, err := intake.NonSessionWorkstreams(repo)
	if err != nil {
		return nil, err
	}
	librarian := librarianWorkstream(repo.Project())
	statuses, err := repo.Statuses()
	if err != nil {
		return nil, err
	}
	for _, status := range statuses {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		id := status.Workstream
		if status.Kind == trace.SessionKind || id == librarian || !slices.Contains(eligible, id) || !hearsay.ValidID(string(id)) {
			continue
		}
		stream := &publishStream{status: status}
		if stream.documents, err = trace.Read[trace.Document](repo, id); err != nil {
			continue
		}
		if stream.transitions, err = trace.Read[trace.Transition](repo, id); err != nil {
			continue
		}
		if stream.evidence, err = lifecycle.MemberEvidenceFrom(stream.documents); err != nil {
			continue
		}
		if stream.approved, err = repo.GroupApproved(id); err != nil {
			continue
		}
		if group, ok, err := repo.PublishedWorkgroupOf(id); err != nil {
			continue
		} else if ok {
			stream.group = &group
		}
		in.streams = append(in.streams, stream)
	}
	return in, nil
}

// projectRef and workstreamRef name the work-source records every owner
// record hangs from.
func (in *publishInputs) projectRef() string {
	return hearsay.Ref(in.workSource, "project/"+string(in.project))
}
func (in *publishInputs) workstreamRef(id config.WorkstreamID) string {
	return hearsay.Ref(in.workSource, "workstream/"+string(id))
}

// projectRecords posts the project on the work source with the owner named as
// its author, so that a project without repos stays readable by the owner.
// The repository is declared only when the project opted in.
func projectRecords(in *publishInputs) []hearsay.Record {
	if !hearsay.ValidID(string(in.project)) {
		return nil
	}
	name := in.cfg.Project.Name
	if strings.TrimSpace(name) == "" {
		name = string(in.project)
	}
	w := hearsay.Wire{Kind: hearsay.KindProject, Project: string(in.project), Name: name, State: coreStarted, Status: "active", Author: in.owner, CreatedAt: hearsay.Time(in.created), UpdatedAt: hearsay.Time(in.created)}
	if in.cfg.Project.HearsayDeclareRepo && in.cfg.Project.Upstream != "" {
		w.Repos = []hearsay.Repo{{Source: "github", Project: in.cfg.Project.Upstream}}
	}
	return []hearsay.Record{{Source: in.workSource, Key: "project/" + string(in.project), Tier: hearsay.TierProject, Versions: []hearsay.Wire{w}}}
}

// workstreamRecords posts each eligible workstream on the work source with
// no author: what the owner asked for, a core state, Osmia's own state as the
// status, the pull request and why it was cancelled or abandoned.
func workstreamRecords(in *publishInputs) []hearsay.Record {
	var out []hearsay.Record
	for _, st := range in.streams {
		id := string(st.id())
		created := st.status.CreatedAt
		w := hearsay.Wire{Kind: hearsay.KindWorkstream, Project: string(in.project), Workstream: id,
			Name: st.title(), Description: st.description(), State: st.coreState(), Status: boundedText(st.osmiaState(), 64),
			CreatedAt: hearsay.Time(created), UpdatedAt: hearsay.Time(st.changedAt())}
		if st.evidence.Published && isWebURL(st.evidence.Publication.URL) {
			w.URL = st.evidence.Publication.URL
		}
		out = append(out, hearsay.Record{Source: in.workSource, Key: "workstream/" + id, Tier: hearsay.TierWorkstream, After: []string{in.projectRef()}, Versions: []hearsay.Wire{w}})
	}
	return out
}

// charterRecords posts every charter revision as a revision of the project's
// charter document on the owner source.
func charterRecords(in *publishInputs) []hearsay.Record {
	var versions []hearsay.Wire
	var created time.Time
	for _, d := range in.documents {
		if d.ID != "charter" || d.Path != "charter.md" {
			continue
		}
		if created.IsZero() {
			created = d.At
		}
		versions = append(versions, hearsay.Wire{Kind: hearsay.KindDocument, Project: string(in.project), Document: charterDocument, Name: "Charter", Body: d.Content, Author: in.owner, CreatedAt: hearsay.Time(created), UpdatedAt: hearsay.Time(d.At)})
	}
	if len(versions) == 0 {
		return nil
	}
	return []hearsay.Record{{Source: in.ownerSource, Key: "document/" + charterDocument, Tier: hearsay.TierRecord, After: []string{in.projectRef()}, Versions: versions}}
}

const charterDocument = "charter"

// sealedSpecDocument is the ID of a workstream's spec document. Document ids
// are project-wide, so it carries the workstream's.
func sealedSpecDocument(id config.WorkstreamID) string { return string(id) + "-spec" }

// sealedSpecRecords posts each seal of a workstream as a revision of the
// workstream's spec document on the owner source: the spec revision the seal
// ratified. A seal awaiting its workgroup's approval is not yet ratified.
func sealedSpecRecords(in *publishInputs) []hearsay.Record {
	var out []hearsay.Record
	for _, st := range in.streams {
		id := st.id()
		versions := st.sealedSpecs(in)
		if len(versions) == 0 || !hearsay.ValidID(sealedSpecDocument(id)) {
			continue
		}
		out = append(out, hearsay.Record{Source: in.ownerSource, Key: "document/" + sealedSpecDocument(id), Tier: hearsay.TierRecord, After: []string{in.workstreamRef(id)}, Versions: versions})
	}
	return out
}

// sealedSpecs is the workstream's spec document revisions, one per seal,
// oldest first.
func (st *publishStream) sealedSpecs(in *publishInputs) []hearsay.Wire {
	if !st.approved {
		return nil
	}
	latest := map[int]trace.Document{}
	for _, d := range st.documents {
		if d.ID == seal.DocumentID && d.Path == seal.Path {
			if parsed, err := seal.Parse([]byte(d.Content)); err == nil {
				latest[parsed.Seal] = d
			}
		}
	}
	numbers := make([]int, 0, len(latest))
	for n := range latest {
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	var versions []hearsay.Wire
	var created time.Time
	for _, n := range numbers {
		d := latest[n]
		parsed, _ := seal.Parse([]byte(d.Content))
		spec, ok := st.specRevision(parsed.Revision.Spec)
		if !ok {
			continue
		}
		if created.IsZero() {
			created = d.At
		}
		versions = append(versions, hearsay.Wire{Kind: hearsay.KindDocument, Project: string(in.project), Workstream: string(st.id()), Document: sealedSpecDocument(st.id()),
			Name: boundedText("Spec: "+specTitle(spec, string(st.id())), 200), Body: spec, Author: in.owner, CreatedAt: hearsay.Time(created), UpdatedAt: hearsay.Time(d.At)})
	}
	return versions
}

// specRevision is the content of one recorded revision of spec.md.
func (st *publishStream) specRevision(revision int) (string, bool) {
	for _, d := range st.documents {
		if d.ID == plan.SpecDocument && d.Revision == revision {
			return d.Content, true
		}
	}
	return "", false
}

// workgroupSpecDocument is the ID of a workgroup's project spec document.
func workgroupSpecDocument(id config.WorkgroupID) string { return string(id) + "-project-spec" }

// workgroupSpecRecords posts a workgroup's owner-approved project spec once
// as a document on the project, on the owner source. Every member project
// posts it on its own project.
func workgroupSpecRecords(in *publishInputs) []hearsay.Record {
	var out []hearsay.Record
	seen := map[config.WorkgroupID]bool{}
	for _, st := range in.streams {
		g := st.group
		if g == nil || g.Spec == nil || seen[g.ID] || !hearsay.ValidID(workgroupSpecDocument(g.ID)) {
			continue
		}
		seen[g.ID] = true
		w := hearsay.Wire{Kind: hearsay.KindDocument, Project: string(in.project), Document: workgroupSpecDocument(g.ID), Name: "Workgroup project spec", Body: renderProjectSpec(*g), Author: in.owner,
			CreatedAt: hearsay.Time(g.ApprovedAt), UpdatedAt: hearsay.Time(g.UpdatedAt)}
		out = append(out, hearsay.Record{Source: in.ownerSource, Key: "document/" + workgroupSpecDocument(g.ID), Tier: hearsay.TierRecord, After: []string{in.projectRef()}, Versions: []hearsay.Wire{w}})
	}
	return out
}

// renderProjectSpec renders the shared part of a project spec: its intended
// behaviour, the contracts at the seams between repos and the workstream
// graph. Each member's own slice and plan stay out.
func renderProjectSpec(g trace.PublishedWorkgroup) string {
	spec := g.Spec
	var b strings.Builder
	fmt.Fprintf(&b, "# Workgroup project spec\n\nWorkgroup %s. Members: %s.\n\n## Behaviour\n\n%s\n", g.ID, joinStreams(g.Members), strings.TrimSpace(spec.Behavior))
	if len(spec.Contracts) > 0 {
		b.WriteString("\n## Contracts\n")
		for _, c := range spec.Contracts {
			fmt.Fprintf(&b, "\n### %s\n\n%s\n", strings.TrimSpace(c.Name), strings.TrimSpace(c.Text))
		}
	}
	if len(spec.Graph) > 0 {
		b.WriteString("\n## Workstream graph\n\n")
		for _, e := range spec.Graph {
			fmt.Fprintf(&b, "- %s -> %s (%s)\n", e.From, e.To, e.Type)
		}
	}
	return b.String()
}

func joinStreams(ids []config.WorkstreamID) string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	return strings.Join(names, ", ")
}

// osmiaState is Osmia's own state for the workstream.
func (st *publishStream) osmiaState() string {
	if st.status.State == "" {
		return "new"
	}
	return st.status.State
}

// coreState maps Osmia's workflow state to Hearsay's core states: a workstream
// that is being shaped or awaits sealing is proposed; an approved, building
// or assembled one is started; one waiting on an owner decision is blocked;
// a delivered one is completed; an abandoned one is abandoned.
func (st *publishStream) coreState() string {
	switch state := st.status.State; state {
	case intake.AbandonedState:
		return coreAbandoned
	case intake.DeliveredState:
		return coreCompleted
	default:
		if len(st.status.Gates) > 0 {
			return coreBlocked
		}
		switch state {
		case intake.RatifiedState, intake.BuildingState, intake.AssembledState:
			return coreStarted
		}
		return coreProposed
	}
}

// handed is the text the owner handed in, when the workstream began as a
// hand-in.
func (st *publishStream) handed() string {
	var text string
	var revision int
	for _, d := range st.documents {
		if d.ID == intake.HandedDocument && d.Revision >= revision {
			text, revision = strings.TrimSpace(d.Content), d.Revision
		}
	}
	return text
}

// title is the workstream's name: the heading of its latest sealed spec, else
// the first line of what the owner handed in, else its ID.
func (st *publishStream) title() string {
	latest, found := -1, ""
	for _, d := range st.documents {
		if d.ID == seal.DocumentID && d.Path == seal.Path {
			if parsed, err := seal.Parse([]byte(d.Content)); err == nil && parsed.Seal > latest {
				if spec, ok := st.specRevision(parsed.Revision.Spec); ok {
					latest, found = parsed.Seal, spec
				}
			}
		}
	}
	if found != "" && st.approved {
		if t := specTitle(found, ""); t != "" {
			return boundedText(t, 200)
		}
	}
	for _, line := range strings.Split(st.handed(), "\n") {
		if t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#")); t != "" {
			return boundedText(t, 200)
		}
	}
	return string(st.id())
}

// specTitle is the text of the first level-one heading of a spec, or fallback.
func specTitle(spec, fallback string) string {
	for _, line := range strings.Split(spec, "\n") {
		if t, ok := strings.CutPrefix(line, "# "); ok && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	return fallback
}

// description is what the owner asked for, followed by what is known of the
// workgroup, the pull request and the cancellation. It never carries the
// pull request's description.
func (st *publishStream) description() string {
	var parts []string
	if ask := st.handed(); ask != "" {
		parts = append(parts, boundedText(ask, descriptionLimit))
	}
	if g := st.group; g != nil {
		var others []string
		for _, m := range g.Members {
			if m != st.id() {
				others = append(others, string(m))
			}
		}
		parts = append(parts, fmt.Sprintf("Part of workgroup %s with %s.", g.ID, strings.Join(others, ", ")))
	}
	e := st.evidence
	switch {
	case e.Published:
		state := "open"
		switch {
		case e.Merged():
			state = "merged"
		case e.Outcome.PullRequest == e.Publication.PullRequest && e.Outcome.Outcome == "closed":
			state = "closed without merging"
		}
		parts = append(parts, fmt.Sprintf("Pull request: %s (%s).", e.Publication.URL, state))
	case e.OwnerMerge:
		parts = append(parts, "Merged by the owner outside a pull request.")
	}
	if st.status.State == intake.AbandonedState {
		if reason := st.abandonReason(); reason != "" {
			parts = append(parts, "Cancelled: "+reason)
		} else {
			parts = append(parts, "Cancelled.")
		}
	}
	return strings.Join(parts, "\n\n")
}

// abandonReason is the reason the owner gave when abandoning the workstream.
func (st *publishStream) abandonReason() string {
	for _, t := range slices.Backward(st.transitions) {
		if t.ID == intake.AbandonTransition && t.Subject == trace.FeatureSubject && t.To == intake.AbandonedState {
			return strings.TrimSpace(t.Reason)
		}
	}
	return ""
}

// changedAt is when the workstream's published facts last changed in the
// trace: its creation, its latest feature transition, and the delivery
// records.
func (st *publishStream) changedAt() time.Time {
	at := st.status.CreatedAt
	for _, t := range st.transitions {
		if t.Subject == trace.FeatureSubject && t.At.After(at) {
			at = t.At
		}
	}
	for _, d := range st.documents {
		switch d.ID {
		case intake.HandedDocument, "final-publication", "own-pull-request-outcome", "final-merge", seal.DocumentID:
			if d.At.After(at) {
				at = d.At
			}
		}
	}
	return at
}

func isWebURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// boundedText cuts s to at most limit bytes on a character boundary, marking
// a cut.
func boundedText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
