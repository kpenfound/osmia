package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	publishInterval  = 15 * time.Second
	publishBackoff   = time.Minute
	descriptionLimit = 16 << 10
)

const (
	coreProposed  = "proposed"
	coreStarted   = "started"
	coreBlocked   = "blocked"
	coreCompleted = "completed"
	coreAbandoned = "abandoned"
)

type publishInputs struct {
	cfg                     *config.Config
	owner                   string
	workSource, ownerSource string
	project                 config.ProjectID
	created                 time.Time
	documents               []trace.Document
	streams                 []*publishStream
}

type publishStream struct {
	status      trace.WorkstreamStatus
	documents   []trace.Document
	transitions []trace.Transition
	evidence    lifecycle.MemberEvidence
	group       *trace.PublishedWorkgroup
	approved    bool

	questions     []trace.QuestionState
	rulings       []trace.Ruling
	amendments    []trace.Amendment
	assumptions   []trace.Assumption
	followups     []followup.Unit
	decisionsRead bool
}

func (s *publishStream) id() config.WorkstreamID { return s.status.Workstream }

type publishSource func(*publishInputs) []hearsay.Record

var publishSources = []publishSource{projectRecords, workstreamRecords, charterRecords, sealedSpecRecords, workgroupSpecRecords, decisionRecords, mapUpdateRecords}

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
			s.hearsayLoopPassed()
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
		s.hearsayLoopPassed()
	}
}

func (s *Service) hearsayLoopPassed() {
	if passed := s.options.hearsayPassed; passed != nil {
		passed()
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
	return s.postRecords(ctx, repo, cfg, token, records)
}

func (s *Service) postRecords(ctx context.Context, repo *trace.Repository, cfg *config.Config, token string, live []hearsay.Record) (hearsay.Result, error) {
	kept, keptErr := keptRecords(repo, live)
	records := append(slices.Clone(live), kept...)
	stored, err := repo.HearsayPosted()
	if err != nil {
		return hearsay.Result{}, err
	}
	posted := make(map[string]hearsay.Posted, len(stored))
	for _, p := range stored {
		posted[hearsay.Ref(p.Source, p.Key)] = hearsay.Posted{Digest: p.Digest, UpdatedAt: p.UpdatedAt}
	}
	res := hearsay.Reconcile(ctx, hearsay.Poster{URL: cfg.Hearsay.Publish.HooksURL, Token: token}, records, posted)
	var entries []trace.PostedRecord
	for _, e := range res.Entries {
		entries = append(entries, trace.PostedRecord{Source: e.Source, Key: e.Key, Digest: e.Digest, UpdatedAt: e.UpdatedAt})
		posted[hearsay.Ref(e.Source, e.Key)] = e.Posted
	}
	if err := repo.RecordHearsayPosted(context.WithoutCancel(ctx), entries, s.now()); err != nil {
		return res, fmt.Errorf("recording posted digests: %w", err)
	}
	if err := releaseKept(context.WithoutCancel(ctx), repo, kept, posted, s.now()); err != nil {
		return res, fmt.Errorf("releasing kept records: %w", err)
	}
	return res, keptErr
}

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
	statuses, err := repo.Statuses()
	if err != nil {
		return nil, err
	}
	for _, status := range statuses {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		id := status.Workstream
		if status.Kind == trace.SessionKind || !slices.Contains(eligible, id) || !hearsay.ValidID(string(id)) {
			continue
		}
		stream, err := readPublishStream(repo, status)
		if err != nil {
			continue
		}
		in.streams = append(in.streams, stream)
	}
	return in, nil
}

func readPublishStream(repo *trace.Repository, status trace.WorkstreamStatus) (*publishStream, error) {
	id := status.Workstream
	stream := &publishStream{status: status}
	var err error
	if stream.documents, err = trace.Read[trace.Document](repo, id); err != nil {
		return nil, err
	}
	if stream.transitions, err = trace.Read[trace.Transition](repo, id); err != nil {
		return nil, err
	}
	if stream.evidence, err = lifecycle.MemberEvidenceFrom(stream.documents); err != nil {
		return nil, err
	}
	if stream.approved, err = repo.GroupApproved(id); err != nil {
		return nil, err
	}
	if group, ok, err := repo.PublishedWorkgroupOf(id); err != nil {
		return nil, err
	} else if ok {
		stream.group = &group
	}
	_ = stream.readDecisions(repo)
	return stream, nil
}

func (in *publishInputs) projectRef() string {
	return hearsay.Ref(in.workSource, "project/"+string(in.project))
}
func (in *publishInputs) workstreamRef(id config.WorkstreamID) string {
	return hearsay.Ref(in.workSource, "workstream/"+string(id))
}

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

func sealedSpecDocument(id config.WorkstreamID) string { return string(id) + "-spec" }
func jevSpecDocument(id config.WorkstreamID) string    { return string(id) + "-jev-spec" }

func sealedSpecRecords(in *publishInputs) []hearsay.Record {
	var out []hearsay.Record
	for _, st := range in.streams {
		id := st.id()
		var owned, jev []hearsay.Wire
		for _, v := range st.sealedSpecs(in) {
			if v.by == byJev {
				jev = append(jev, v.wire)
			} else {
				owned = append(owned, v.wire)
			}
		}
		if len(owned) > 0 && hearsay.ValidID(sealedSpecDocument(id)) {
			out = append(out, hearsay.Record{Source: in.ownerSource, Key: "document/" + sealedSpecDocument(id), Tier: hearsay.TierRecord, After: []string{in.workstreamRef(id)}, Versions: owned})
		}
		if len(jev) > 0 && hearsay.ValidID(jevSpecDocument(id)) {
			out = append(out, hearsay.Record{Source: in.workSource, Key: "document/" + jevSpecDocument(id), Tier: hearsay.TierRecord, After: []string{in.workstreamRef(id)}, Versions: jev})
		}
	}
	return out
}

type sealedSpec struct {
	wire     hearsay.Wire
	document string
	by       decider
}

func (st *publishStream) sealedSpecs(in *publishInputs) []sealedSpec {
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
	ratifiers := st.sealRatifiers(numbers, latest)
	var versions []sealedSpec
	var ownerCreated, jevCreated time.Time
	for _, n := range numbers {
		d := latest[n]
		parsed, _ := seal.Parse([]byte(d.Content))
		spec, ok := st.specRevision(parsed.Revision.Spec)
		if !ok {
			continue
		}
		w := hearsay.Wire{Kind: hearsay.KindDocument, Project: string(in.project), Workstream: string(st.id()), UpdatedAt: hearsay.Time(d.At)}
		v := sealedSpec{by: ratifiers[n]}
		name := "Spec: " + specTitle(spec, string(st.id()))
		if v.by == byJev {
			if jevCreated.IsZero() {
				jevCreated = d.At
			}
			w.Document, w.CreatedAt = jevSpecDocument(st.id()), hearsay.Time(jevCreated)
			name = "Spec ratified by Jev: " + specTitle(spec, string(st.id()))
		} else {
			if ownerCreated.IsZero() {
				ownerCreated = d.At
			}
			w.Document, w.CreatedAt, w.Author = sealedSpecDocument(st.id()), hearsay.Time(ownerCreated), in.owner
		}
		w.Name, w.Body = boundedText(name, 200), spec
		v.wire, v.document = w, w.Document
		versions = append(versions, v)
	}
	return versions
}

func (st *publishStream) sealRatifiers(numbers []int, latest map[int]trace.Document) map[int]decider {
	byAmendment := map[int]decider{}
	inherits := map[int]bool{}
	applied := map[string]trace.Document{}
	for _, d := range st.documents {
		if id, ok := amendmentOf(d.Path, "application.json"); ok && d.Revision >= applied[id].Revision {
			applied[id] = d
		}
	}
	for _, d := range st.documents {
		if !strings.HasPrefix(d.Path, "tools/amendment-group-") || !strings.HasSuffix(d.Path, "-application.json") {
			continue
		}
		var a amendment.Application
		if json.Unmarshal([]byte(d.Content), &a) == nil && a.To.Seal > 0 {
			byAmendment[a.To.Seal] = byOwner
		}
	}
	for id, doc := range applied {
		var a amendment.Application
		if json.Unmarshal([]byte(doc.Content), &a) != nil || a.To.Seal < 1 {
			continue
		}
		by, ok := st.amendmentDeciderOf(id)
		switch {
		case !ok:
		case by == byChief:
			inherits[a.To.Seal] = true
		default:
			byAmendment[a.To.Seal] = by
		}
	}
	out := map[int]decider{}
	previous := byOwner
	for _, n := range numbers {
		by := byOwner
		switch {
		case byAmendment[n] != byChief:
			by = byAmendment[n]
		case inherits[n]:
			by = previous
		default:
			by = st.roundRatifier(latest[n])
		}
		out[n], previous = by, by
	}
	return out
}

func (st *publishStream) roundRatifier(sealDoc trace.Document) decider {
	parsed, err := seal.Parse([]byte(sealDoc.Content))
	if err != nil {
		return byOwner
	}
	var latest trace.Document
	for _, d := range st.documents {
		if d.Path == shed.RatificationPath(parsed.Round) && d.Revision >= latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return byOwner
	}
	var r shed.Ratification
	if json.Unmarshal([]byte(latest.Content), &r) != nil || !trace.ValidDecisionActor(latest) {
		return byOwner
	}
	if jevDecider(r.DecidedBy, r.Judgment) && latest.Actor == trace.JevActor {
		return byJev
	}
	return byOwner
}

func (st *publishStream) amendmentDeciderOf(id string) (decider, bool) {
	var final trace.Document
	var verdict intake.AmendmentDecision
	for _, d := range st.documents {
		if d.Path != intake.AmendmentDecisionPath(id) || d.Revision < final.Revision {
			continue
		}
		var v intake.AmendmentDecision
		if json.Unmarshal([]byte(d.Content), &v) != nil || v.Decision == intake.AmendmentRound {
			continue
		}
		final, verdict = d, v
	}
	if final.Revision == 0 {
		return 0, false
	}
	return amendmentDecider(final, verdict)
}

func amendmentOf(path, file string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "amendments/")
	if !ok {
		return "", false
	}
	id, name, ok := strings.Cut(rest, "/")
	return id, ok && name == file && id != ""
}

func (st *publishStream) specRevision(revision int) (string, bool) {
	for _, d := range st.documents {
		if d.ID == plan.SpecDocument && d.Revision == revision {
			return d.Content, true
		}
	}
	return "", false
}

func workgroupSpecDocument(id config.WorkgroupID) string { return string(id) + "-project-spec" }

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

func (st *publishStream) osmiaState() string {
	if st.status.State == "" {
		return "new"
	}
	return st.status.State
}

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

func specTitle(spec, fallback string) string {
	for _, line := range strings.Split(spec, "\n") {
		if t, ok := strings.CutPrefix(line, "# "); ok && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	return fallback
}

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

func (st *publishStream) abandonReason() string {
	for _, t := range slices.Backward(st.transitions) {
		if t.ID == intake.AbandonTransition && t.Subject == trace.FeatureSubject && t.To == intake.AbandonedState {
			return strings.TrimSpace(t.Reason)
		}
	}
	return ""
}

func (st *publishStream) changedAt() time.Time {
	at := st.status.CreatedAt
	for _, t := range st.transitions {
		if t.Subject == trace.FeatureSubject && t.At.After(at) {
			at = t.At
		}
	}
	for _, d := range st.documents {
		switch d.ID {
		case intake.HandedDocument, "final-publication", "own-pull-request-outcome", "final-merge", "final-landing", seal.DocumentID:
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
