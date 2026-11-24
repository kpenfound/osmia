package lifecycle

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	evidenceIntentBytes       = 3 << 10
	evidenceLineBytes         = 600
	evidenceEntryBytes        = 3 << 10
	evidenceAcceptanceBytes   = 10 << 10
	evidenceContractBytes     = 6 << 10
	evidenceReliantBytes      = 6 << 10
	evidenceDeliverableBytes  = 8 << 10
	evidenceOutcomeBytes      = 3 << 10
	evidenceFindingBytes      = 10 << 10
	evidenceMoveBytes         = 8 << 10
	evidenceMovesShown        = 10
	evidenceDeliverableAbout  = 240
	evidenceFinalSummaryBytes = 600
)

var (
	backtickFence = regexp.MustCompile("(?s)```.*?(```|$)")
	tildeFence    = regexp.MustCompile("(?s)~~~.*?(~~~|$)")
	headingLine   = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t#]*$`)
	fenceLine     = regexp.MustCompile("^ {0,3}(```|~~~)")
)

func oneLine(text string, limit int) (string, bool) {
	text = backtickFence.ReplaceAllString(text, "[code omitted]")
	text = tildeFence.ReplaceAllString(text, "[code omitted]")
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " […cut]", true
}

type sectionCount struct {
	Total, Shown, Shortened int
}

func packSection(kind, id, title string, lines []string, limit int) ([]router.Entry, sectionCount) {
	count := sectionCount{Total: len(lines)}
	var entries []router.Entry
	var body []string
	size, used := 0, 0
	flush := func() {
		if len(body) > 0 {
			entries = append(entries, router.Entry{Kind: kind, ID: id, Text: strings.Join(body, "\n")})
		}
		body, size = nil, 0
	}
	for _, line := range lines {
		text, cut := oneLine(line, evidenceLineBytes)
		if used+len(text)+1 > limit {
			break
		}
		if size+len(text)+1 > evidenceEntryBytes {
			flush()
		}
		body = append(body, text)
		size += len(text) + 1
		used += len(text) + 1
		count.Shown++
		if cut {
			count.Shortened++
		}
	}
	flush()
	if len(entries) == 0 {
		count.Total = max(count.Total, len(lines))
	}
	head := fmt.Sprintf("%s: %d on record", title, count.Total)
	if len(entries) == 0 {
		return []router.Entry{{Kind: kind, ID: id, Text: head}}, count
	}
	entries[0].Text = head + "\n" + entries[0].Text
	if count.Shown < count.Total || count.Shortened > 0 {
		note := fmt.Sprintf("[cut: %d of %d %s shown", count.Shown, count.Total, title)
		if count.Shortened > 0 {
			note += fmt.Sprintf("; %d of them shortened to %d bytes", count.Shortened, evidenceLineBytes)
		}
		last := &entries[len(entries)-1]
		last.Text += "\n" + note + "]"
	}
	return entries, count
}

func missingEntry(kind, id, text string) router.Entry {
	return router.Entry{Kind: kind, ID: id, Text: "missing from the records: " + text}
}

func markdownSection(content, title string) (string, bool) {
	var (
		level int
		out   []string
		code  bool
		found bool
	)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if fenceLine.MatchString(line) {
			code = !code
		}
		if !code {
			if h := headingLine.FindStringSubmatch(line); h != nil {
				depth := len(h[1])
				switch {
				case level > 0 && depth <= level:
					return strings.TrimSpace(strings.Join(out, "\n")), true
				case level == 0 && strings.EqualFold(strings.TrimSpace(h[2]), title):
					level, found = depth, true
				}
				continue
			}
		}
		if level > 0 {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n")), found
}

func specIntent(content string) (string, bool) {
	if text, found := markdownSection(content, "Intent"); found && text != "" {
		return text, true
	}
	var head []string
	code := false
	for _, line := range strings.Split(content, "\n") {
		if fenceLine.MatchString(line) {
			code = !code
		}
		if h := headingLine.FindStringSubmatch(line); !code && h != nil && strings.EqualFold(strings.TrimSpace(h[2]), plan.CriteriaHeading) {
			break
		}
		head = append(head, strings.TrimRight(line, "\r"))
	}
	return strings.TrimSpace(strings.Join(head, "\n")), false
}

type sealedSpec struct {
	Stream         config.WorkstreamID
	Seal, Revision int
	Intent         string
	IntentSection  bool
	Criteria       []plan.Criterion
	Plan           *plan.Plan
	Missing        []string
}

func readSealedSpec(repo *trace.Repository, stream config.WorkstreamID) (sealedSpec, error) {
	out := sealedSpec{Stream: stream}
	latest, _, found, err := seal.Latest(repo, stream)
	if err != nil {
		return out, err
	}
	if !found {
		out.Missing = append(out.Missing, fmt.Sprintf("workstream %s has no sealed spec or plan", stream))
		return out, nil
	}
	out.Seal, out.Revision = latest.Seal, latest.Revision.Spec
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return out, err
	}
	if i := slices.IndexFunc(docs, func(d trace.Document) bool {
		return d.ID == plan.SpecDocument && d.Revision == latest.Revision.Spec
	}); i < 0 {
		out.Missing = append(out.Missing, fmt.Sprintf("spec.md revision %d of workstream %s", latest.Revision.Spec, stream))
	} else {
		out.Intent, out.IntentSection = specIntent(docs[i].Content)
		out.Criteria = plan.ParseSpec(docs[i].Content).Criteria
	}
	graph, err := intake.SealedPlan(repo, stream, latest.Revision.Plan)
	if err != nil {
		out.Missing = append(out.Missing, fmt.Sprintf("plan.json revision %d of workstream %s", latest.Revision.Plan, stream))
		return out, nil
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		out.Missing = append(out.Missing, fmt.Sprintf("plan.json revision %d of workstream %s, which cannot be read", latest.Revision.Plan, stream))
		return out, nil
	}
	out.Plan = &p
	return out, nil
}

func (sp sealedSpec) intentText(what string) (string, bool) {
	if sp.Intent == "" {
		reason := "its spec states none"
		if len(sp.Missing) > 0 {
			reason = "missing from the records: " + strings.Join(sp.Missing, "; ")
		}
		return fmt.Sprintf("The sealed intent of %s is not known: %s.", what, reason), false
	}
	text, cut := oneLine(sp.Intent, evidenceIntentBytes)
	from := "its Intent section"
	if !sp.IntentSection {
		from = "the text before its acceptance criteria, as it has no Intent section"
	}
	return fmt.Sprintf("Sealed intent of %s (from %s; spec revision %d, seal %d): %s", what, from, sp.Revision, sp.Seal, text), cut
}

func (sp sealedSpec) acceptance(kind, what string) ([]router.Entry, sectionCount) {
	if len(sp.Criteria) == 0 {
		if sp.Revision == 0 || len(sp.Missing) > 0 {
			return []router.Entry{missingEntry(kind, string(sp.Stream), "the acceptance outcomes of "+what+": "+strings.Join(sp.Missing, "; "))}, sectionCount{}
		}
		return []router.Entry{{Kind: kind, ID: string(sp.Stream), Text: "Acceptance outcomes of " + what + ": none on record in its sealed spec"}}, sectionCount{}
	}
	lines := make([]string, 0, len(sp.Criteria))
	for _, c := range sp.Criteria {
		lines = append(lines, fmt.Sprintf("%s: %s", plan.Cite(c.Number), c.Text))
	}
	return packSection(kind, string(sp.Stream), "acceptance outcomes of "+what, lines, evidenceAcceptanceBytes)
}

func (s *Controller) dependencyIntent(repo *trace.Repository, stream config.WorkstreamID, c dependencyCase) (string, []router.Entry, map[string]any) {
	facts := map[string]any{}
	dependent := fmt.Sprintf("the dependent workstream %s", stream)
	spec, err := readSealedSpec(repo, stream)
	if err != nil {
		spec = sealedSpec{Stream: stream, Missing: []string{"the dependent's records cannot be read: " + oneLineError(err)}}
	}
	intent, intentCut := spec.intentText(dependent)
	facts["dependent_intent_cut"] = intentCut
	var entries []router.Entry

	acceptance, count := spec.acceptance("dependent acceptance", dependent)
	entries = append(entries, acceptance...)
	facts["dependent_acceptance_outcomes"], facts["dependent_acceptance_shown"] = count.Total, count.Shown

	project, _, projectFound, err := repo.ProjectSpecOf(stream)
	var contracts []string
	var criteria []int
	switch {
	case err != nil:
		entries = append(entries, missingEntry("dependent contracts", string(stream), "the shared project spec cannot be read: "+oneLineError(err)))
	case !projectFound:
		entries = append(entries, missingEntry("dependent contracts", string(stream), "the dependent has no shared project spec, so no contracts"))
	default:
		for _, k := range project.Contracts {
			contracts = append(contracts, fmt.Sprintf("contract %s: %s", k.Name, k.Text))
		}
		packed, n := packSection("dependent contracts", string(stream), "contracts of the shared project spec", contracts, evidenceContractBytes)
		entries = append(entries, packed...)
		facts["contracts"], facts["contracts_shown"] = n.Total, n.Shown
		var seams []string
		if slice, ok := trace.SliceFor(project, stream); ok {
			criteria = slice.ContractCriteria
			for _, number := range criteria {
				seams = append(seams, plan.Cite(number)+" is a contract section of the dependent's own slice, read-only to it")
			}
		}
		for _, e := range project.Graph {
			if e.To == stream || e.From == stream {
				seams = append(seams, fmt.Sprintf("edge from %s to %s of type %s", e.From, e.To, e.Type))
			}
		}
		packed, _ = packSection("dependent contracts", string(stream), "contract sections and edges of the dependent", seams, evidenceContractBytes)
		entries = append(entries, packed...)
	}

	reliant, total, reliantCount := reliantUnits(spec, c, project.Contracts, criteria)
	entries = append(entries, reliant...)
	facts["dependent_plan_units"], facts["relying_units"], facts["relying_units_shown"] = total, reliantCount.Total, reliantCount.Shown

	deliverables, outcome := s.upstreamEvidence(repo, c, facts)
	entries = append(entries, deliverables...)
	entries = append(entries, outcome)
	return intent, entries, facts
}

func oneLineError(err error) string {
	text, _ := oneLine(err.Error(), 200)
	return text
}

func reliantUnits(spec sealedSpec, c dependencyCase, contracts []trace.Contract, criteria []int) ([]router.Entry, int, sectionCount) {
	const kind = "relying units"
	if spec.Plan == nil {
		why := "the dependent has no sealed plan"
		if len(spec.Missing) > 0 {
			why = strings.Join(spec.Missing, "; ")
		}
		return []router.Entry{missingEntry(kind, string(spec.Stream), "the plan units that rely on the upstream: "+why)}, 0, sectionCount{}
	}
	var lines []string
	for _, u := range spec.Plan.Units {
		var reasons []string
		if c.base() {
			reasons = append(reasons, "the unit builds on the base workstream's branch")
		}
		for _, n := range criteria {
			if u.Serves(n) {
				reasons = append(reasons, "serves contract section "+plan.Cite(n))
			}
		}
		text := strings.ToLower(strings.Join(append([]string{u.ID, u.Title, u.Task}, append(u.Acceptance, append(u.Constraints, append(u.Boundaries, u.Guidance...)...)...)...), "\n"))
		if strings.Contains(text, strings.ToLower(string(c.Upstream))) {
			reasons = append(reasons, "names the upstream workstream")
		}
		for _, k := range contracts {
			if k.Name != "" && strings.Contains(text, strings.ToLower(k.Name)) {
				reasons = append(reasons, fmt.Sprintf("names contract %q", k.Name))
			}
		}
		if len(reasons) == 0 {
			continue
		}
		title := u.Title
		if title == "" {
			title, _ = oneLine(u.Task, evidenceDeliverableAbout)
		}
		lines = append(lines, fmt.Sprintf("unit %s (%s): %s", u.ID, title, strings.Join(reasons, "; ")))
	}
	total := len(spec.Plan.Units)
	if len(lines) == 0 {
		return []router.Entry{{Kind: kind, ID: string(spec.Stream), Text: fmt.Sprintf("Plan units that rely on the upstream: none of the %d units of the dependent's sealed plan serves a contract section or names the upstream or a contract; the dependency still parks the whole dependent", total)}}, total, sectionCount{}
	}
	entries, count := packSection(kind, string(spec.Stream), fmt.Sprintf("plan units that rely on the upstream (of %d units in the plan)", total), lines, evidenceReliantBytes)
	return entries, total, count
}

func (s *Controller) upstreamEvidence(repo *trace.Repository, c dependencyCase, facts map[string]any) ([]router.Entry, router.Entry) {
	const deliverableKind, outcomeKind = "upstream planned deliverables", "upstream final outcome"
	id := string(c.Upstream)
	up, ok := s.upstreamOf(repo, c.Upstream)
	if !ok {
		why := "the upstream's records cannot be read now"
		return []router.Entry{missingEntry(deliverableKind, id, "the upstream's plan: "+why)}, missingEntry(outcomeKind, id, "the upstream's final outcome: "+why)
	}

	var deliverables []router.Entry
	spec, err := readSealedSpec(up, c.Upstream)
	var units []plan.Unit
	sealedPlan := false
	switch {
	case err != nil:
		deliverables = []router.Entry{missingEntry(deliverableKind, id, "the upstream's plan cannot be read: "+oneLineError(err))}
	case spec.Plan != nil:
		units, sealedPlan = spec.Plan.Units, true
	default:
		units = latestPlanUnits(up, c.Upstream)
		if units == nil {
			deliverables = []router.Entry{missingEntry(deliverableKind, id, "the upstream has no plan: "+strings.Join(spec.Missing, "; "))}
		}
	}
	if units != nil {
		var lines []string
		merged := 0
		for _, u := range units {
			title := u.Title
			if title == "" {
				title = "untitled"
			}
			about, _ := oneLine(u.Task, evidenceDeliverableAbout)
			line := fmt.Sprintf("unit %s (%s): %s", u.ID, title, about)
			if len(u.Criteria) > 0 {
				line += "; serves " + strings.Join(u.Criteria, ", ")
			}
			if st, err := up.Workflow(c.Upstream, trace.UnitSubject(u.ID)); err == nil && st.Value != "" {
				line += "; state " + st.Value
				if st.Value == UnitMerged {
					merged++
				}
			}
			lines = append(lines, line)
		}
		title := fmt.Sprintf("planned deliverables of the upstream (units of its plan, %d merged)", merged)
		if !sealedPlan {
			title = fmt.Sprintf("planned deliverables of the upstream (units of its latest plan, which was never sealed, %d merged)", merged)
		}
		var n sectionCount
		deliverables, n = packSection(deliverableKind, id, title, lines, evidenceDeliverableBytes)
		facts["upstream_planned_units"], facts["upstream_planned_units_shown"], facts["upstream_units_merged"] = n.Total, n.Shown, merged
	}
	return deliverables, s.upstreamOutcome(up, c, facts)
}

func latestPlanUnits(repo *trace.Repository, stream config.WorkstreamID) []plan.Unit {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil
	}
	var latest *trace.Document
	for i, d := range docs {
		if d.ID == plan.PlanDocument && (latest == nil || d.Revision >= latest.Revision) {
			latest = &docs[i]
		}
	}
	if latest == nil {
		return nil
	}
	p, err := plan.Parse([]byte(latest.Content))
	if err != nil {
		return nil
	}
	return p.Units
}

func (s *Controller) upstreamOutcome(up *trace.Repository, c dependencyCase, facts map[string]any) router.Entry {
	const kind = "upstream final outcome"
	id := string(c.Upstream)
	var lines []string
	recorded := 0
	if transitions, err := trace.Read[trace.Transition](up, c.Upstream); err == nil {
		var last trace.Transition
		for _, t := range transitions {
			if t.Subject == trace.FeatureSubject {
				last = t
			}
		}
		if last.Subject != "" {
			recorded++
			lines = append(lines, fmt.Sprintf("the upstream is %s, since %d minutes: %s", last.To, int(max(s.Now().Sub(last.At), 0)/time.Minute), last.Reason))
			facts["upstream_state"] = last.To
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "the upstream has no workstream state on record")
	}
	if merge, found, err := delivery.ReadDeliveryMerge(up, c.Upstream); err == nil && found {
		recorded++
		lines = append(lines, fmt.Sprintf("its branch %s was merged into %s/%s at commit %s", merge.Branch, merge.Upstream.Remote, merge.Upstream.Branch, merge.Commit))
	} else {
		lines = append(lines, "no merge of its branch is on record")
	}
	if reports, err := delivery.FinalReports(up, c.Upstream); err == nil && len(reports) > 0 {
		recorded++
		last := reports[len(reports)-1]
		blocking := delivery.CountSeverity(last.Findings, delivery.FindingBlocking)
		line := fmt.Sprintf("its latest final report is review %d, outcome %s, with %d blocking and %d advisory findings", last.Review, last.Outcome, blocking, len(last.Findings)-blocking)
		if last.Summary != "" {
			summary, _ := oneLine(last.Summary, evidenceFinalSummaryBytes)
			line += ": " + summary
		}
		lines = append(lines, line)
		facts["upstream_final_reports"] = len(reports)
	} else {
		lines = append(lines, "no final report is on record")
	}
	if recorded == 0 {
		return missingEntry(kind, id, "the upstream's final outcome: it has no workstream state, merge or final report")
	}
	entries, _ := packSection(kind, id, "final outcome of the upstream", lines, evidenceOutcomeBytes)
	return entries[0]
}

func workstreamIntent(repo *trace.Repository, stream config.WorkstreamID) (string, []router.Entry, map[string]any, error) {
	return workstreamSummary(repo, stream, false)
}

func workstreamIntentOrMissing(repo *trace.Repository, stream config.WorkstreamID) (string, []router.Entry, map[string]any) {
	intent, entries, facts, _ := workstreamSummary(repo, stream, true)
	return intent, entries, facts
}

func workstreamSummary(repo *trace.Repository, stream config.WorkstreamID, tolerant bool) (string, []router.Entry, map[string]any, error) {
	facts := map[string]any{}
	what := fmt.Sprintf("workstream %s", stream)
	spec, err := readSealedSpec(repo, stream)
	if err != nil {
		if !tolerant {
			return "", nil, nil, err
		}
		spec = sealedSpec{Stream: stream, Missing: []string{"the workstream's records cannot be read: " + oneLineError(err)}}
	}
	intent, intentCut := spec.intentText(what)
	facts["intent_cut"] = intentCut
	entries, count := spec.acceptance("sealed acceptance", what)
	facts["acceptance_outcomes"], facts["acceptance_shown"] = count.Total, count.Shown
	if tolerant && len(spec.Criteria) > 0 && len(spec.Missing) > 0 {
		entries = append(entries, missingEntry("sealed plan", string(stream), strings.Join(spec.Missing, "; ")))
	}

	reports, err := delivery.FinalReports(repo, stream)
	if err != nil && !tolerant {
		return "", nil, nil, err
	}
	facts["final_reviews"] = len(reports)
	var report *delivery.FinalReport
	for i := len(reports) - 1; i >= 0; i-- {
		if reports[i].Outcome == delivery.FinalReviewed && !reports[i].Maintenance {
			report = &reports[i]
			break
		}
	}
	switch {
	case err != nil:
		entries = append(entries, missingEntry("final review findings", string(stream), "the final reports cannot be read: "+oneLineError(err)))
	case report == nil:
		entries = append(entries, missingEntry("final review findings", string(stream), "no final review has read the workstream, so there are no findings"))
	default:
		findings := slices.Clone(report.Findings)
		sort.SliceStable(findings, func(i, j int) bool {
			return findings[i].Severity == delivery.FindingBlocking && findings[j].Severity != delivery.FindingBlocking
		})
		lines := make([]string, 0, len(findings))
		for _, f := range findings {
			lines = append(lines, fmt.Sprintf("[%s] %s", f.Severity, f.Describe()))
		}
		blocking := delivery.CountSeverity(report.Findings, delivery.FindingBlocking)
		title := fmt.Sprintf("findings of final review %d, the latest (%d blocking, %d advisory; blocking first)", report.Review, blocking, len(report.Findings)-blocking)
		packed, n := packSection("final review findings", string(stream), title, lines, evidenceFindingBytes)
		entries = append(entries, packed...)
		facts["final_review"], facts["findings"], facts["blocking_findings"], facts["findings_shown"] = report.Review, n.Total, blocking, n.Shown
	}

	moves, err := WorkstreamMoves(repo, stream)
	if err != nil {
		if !tolerant {
			return "", nil, nil, err
		}
		entries = append(entries, missingEntry("earlier moves", string(stream), "the earlier moves cannot be read: "+oneLineError(err)))
		return intent, entries, facts, nil
	}
	facts["earlier_moves"] = len(moves)
	shown := moves
	if len(shown) > evidenceMovesShown {
		shown = shown[len(shown)-evidenceMovesShown:]
	}
	facts["earlier_moves_shown"] = len(shown)
	if omitted := len(moves) - len(shown); omitted > 0 {
		entries = append(entries, router.Entry{Kind: "earlier moves", ID: string(stream), Text: fmt.Sprintf("[cut: %d of %d earlier moves shown; the %d oldest are omitted]", len(shown), len(moves), omitted)})
	}
	used := 0
	for _, m := range shown {
		text, _ := oneLine(fmt.Sprintf("%s moved the workstream from %s to %s: %s", m.By, m.From, m.To, m.Note), evidenceLineBytes)
		if used += len(text); used > evidenceMoveBytes {
			entries = append(entries, router.Entry{Kind: "earlier moves", ID: string(stream), Text: "[cut: the remaining earlier moves are omitted]"})
			break
		}
		entries = append(entries, router.Entry{Kind: "earlier move", ID: WorkstreamMoveDocument(m.Move), Text: text})
	}
	return intent, entries, facts, nil
}

func (s *Controller) waitIntent(repo *trace.Repository, stream config.WorkstreamID, c *dependencyCase) (string, []router.Entry, map[string]any) {
	if c == nil {
		return workstreamIntentOrMissing(repo, stream)
	}
	return s.dependencyIntent(repo, stream, *c)
}

func withIntentEvidence(own router.Evidence, intent string, entries []router.Entry, facts map[string]any) router.Evidence {
	if own.Intent == "" {
		own.Intent = intent
	}
	merged := make(map[string]any, len(own.Facts)+len(facts))
	for k, v := range facts {
		merged[k] = v
	}
	for k, v := range own.Facts {
		merged[k] = v
	}
	own.Facts = merged
	own.Records = append(slices.Clone(own.Records), entries...)
	return own
}
