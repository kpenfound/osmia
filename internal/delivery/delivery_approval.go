package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

const DeliveryPath = "final/delivery.json"
const DeliveryDocument = "final-delivery"
const DeliverySubject = "delivery-owner"

// PublishedTitle is the title to publish for an approval: the title it
// recorded, or, for an approval recorded before this field existed,
// DeliveryTitle(Description, stream), which is what was published for it
// then.
func PublishedTitle(approval DeliveryApproval, stream config.WorkstreamID) string {
	if approval.Title != "" {
		return approval.Title
	}
	return DeliveryTitle(approval.Description, stream)
}

func DescriptionHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

// BoundSubject truncates s to subjectLength runes, as DeliveryTitle and a
// landing commit subject do.
func BoundSubject(s string) string {
	if runes := []rune(s); len(runes) > subjectLength {
		return string(append(runes[:subjectLength-1], '…'))
	}
	return s
}

func DeliveryDocuments(repository *trace.Repository, stream config.WorkstreamID) (trace.Document, trace.Document, *DeliveryApproval, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return trace.Document{}, trace.Document{}, nil, err
	}
	var review, decision trace.Document
	for _, d := range docs {
		switch d.ID {
		case FinalReportDocument:
			review = d
		case DeliveryDocument:
			decision = d
		}
	}
	if decision.Revision == 0 {
		return review, decision, nil, nil
	}
	var approval DeliveryApproval
	if err := json.Unmarshal([]byte(decision.Content), &approval); err != nil {
		return review, decision, nil, err
	}
	return review, decision, &approval, nil
}

// HeadingLine matches a Markdown heading line, capturing its text.
var HeadingLine = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)[ \t]*$`)

// FirstHeading returns the text of the first Markdown heading in content, or
// "" if content holds none.
func FirstHeading(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if m := HeadingLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			if title := strings.TrimSpace(m[1]); title != "" {
				return title
			}
		}
	}
	return ""
}

// SummaryStructural matches a Markdown line that SpecSummary excludes from
// the prose it extracts: a heading, a bullet or numbered list item, a code
// fence or an HTML comment.
var SummaryStructural = regexp.MustCompile(`^ {0,3}(#{1,6}[ \t]|[-*+][ \t]|[0-9]{1,9}[.)][ \t]|` + "```" + `|~~~|<!--)`)

// SpecSummaryBound is the longest SpecSummary returns, in runes.
const SpecSummaryBound = 600

// SpecSummary returns a short prose summary of the sealed spec: its first
// paragraph of plain text, skipping headings, list items, fenced code and
// HTML comments, bounded to SpecSummaryBound runes. It is deterministic:
// presenting the same spec revision twice yields the same summary.
func SpecSummary(content string) string {
	var paragraph []string
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(paragraph) > 0 {
				break
			}
			continue
		}
		if SummaryStructural.MatchString(line) {
			continue
		}
		paragraph = append(paragraph, trimmed)
	}
	text := strings.Join(paragraph, " ")
	if runes := []rune(text); len(runes) > SpecSummaryBound {
		text = string(runes[:SpecSummaryBound-1]) + "…"
	}
	return text
}

// WorkstreamTitle is the title shown to the owner and used as the pull
// request title: the chief of staff's latest status goal, the value
// archiveTitle and status also use; when none is recorded, a fallback
// derived from recorded documents (the sealed spec's first heading, or the
// handed issue's title) before the bare workstream ID. It is bounded to the
// same subject length as a landing commit.
func WorkstreamTitle(repository *trace.Repository, stream config.WorkstreamID, spec, handed trace.Document) (string, error) {
	statuses, err := repository.Statuses()
	if err != nil {
		return "", err
	}
	for _, w := range statuses {
		if w.Workstream == stream && w.Status != nil && w.Status.Goal != "" {
			return BoundSubject(w.Status.Goal), nil
		}
	}
	if title := FirstHeading(spec.Content); title != "" {
		return BoundSubject(title), nil
	}
	if title := FirstHeading(handed.Content); title != "" {
		return BoundSubject(title), nil
	}
	return "Workstream " + string(stream), nil
}

// ClosingReference is a GitHub closing reference to the issue handed
// recorded as its source: "closes #N" when the issue is in targetRepo, or
// "closes OWNER/REPO#N" otherwise. It is "" for a workstream handed in from
// a file or stdin, or with no handed document recorded.
func ClosingReference(handed trace.Document, targetRepo string) string {
	if handed.Revision == 0 {
		return ""
	}
	ref, err := issues.Parse(handed.Source)
	if err != nil {
		return ""
	}
	if strings.EqualFold(targetRepo, ref.Owner+"/"+ref.Repo) {
		return fmt.Sprintf("closes #%d", ref.Number)
	}
	return fmt.Sprintf("closes %s/%s#%d", ref.Owner, ref.Repo, ref.Number)
}

// draftDelivery is the workstream's title and pull request body: the title
// (see WorkstreamTitle) and a short prose summary of the sealed spec,
// closing the issue the workstream was handed in from when one is recorded
// and its repository can be compared with the one the pull request would
// target, and the workstream's logged assumptions, when it has any. All of
// it comes from documents already in the trace, so presenting the same
// recorded trace twice produces the same draft, and no agent is called.
func (s *Controller) DraftDelivery(cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID, report FinalReport) (title, body string, err error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", "", err
	}
	var spec, handed trace.Document
	for _, d := range docs {
		switch {
		case d.ID == plan.SpecDocument && d.Revision == report.Spec:
			spec = d
		case d.ID == handedDocument:
			handed = d
		}
	}
	title, err = WorkstreamTitle(repository, stream, spec, handed)
	if err != nil {
		return "", "", err
	}
	summary := SpecSummary(spec.Content)
	if summary == "" {
		summary = title
	}
	targetRepo := cfg.Project.Upstream
	if report.Upstream != nil && report.Upstream.Workstream != "" {
		targetRepo = cfg.Project.PushRepository()
	}
	body = summary + "\n"
	if ref := ClosingReference(handed, targetRepo); ref != "" {
		body += "\n" + ref + "\n"
	}
	// A workgroup merge-order edge whose upstream has not yet merged keeps
	// the pull request a draft; the owner sees the dependency stated here,
	// as part of the one delivery approval, so marking it ready later needs
	// no separate owner prompt (spec#12).
	if references, err := s.PendingMergeOrderReferences(repository, stream); err != nil {
		return "", "", err
	} else if len(references) > 0 {
		body += fmt.Sprintf("\nThis depends on %s, which must merge first. This pull request opens as a draft. After updating to the merged dependencies, checks, final review and a fresh owner delivery approval are required before marking it ready.\n", strings.Join(references, ", "))
	}
	assumptions, err := trace.Assumptions(repository, stream)
	if err != nil {
		return "", "", err
	}
	if len(assumptions) > 0 {
		body += "\nAssumptions:\n"
		for _, a := range assumptions {
			body += "- " + strings.TrimSpace(a.Text) + "\n"
		}
	}
	return title, body, nil
}

func (s *Controller) DeliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return DeliveryPresentation{}, api
	}
	return s.PresentDelivery(ctx, project, stream, repository)
}

// presentDelivery builds the presentation of one workstream of the project.
func (s *Controller) PresentDelivery(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository) (DeliveryPresentation, *APIError) {
	if pending, err := s.UnresolvedWork(repository, stream); err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read discovered work"}
	} else if pending {
		return DeliveryPresentation{}, &APIError{Code: Conflict, Message: "necessary discovered work or a plan revision remains unresolved"}
	}

	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read feature state"}
	}
	published, err := Publications(repository, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read publication records"}
	}
	var publication *DeliveryPublication
	if len(published) > 0 {
		publication = &published[len(published)-1]
	}
	maintenance := false
	if feature.Value == DeliveredState {
		maintenance, err = s.UpstreamDeliveryPending(repository, s.About(repository), stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read dependency integration"}
		}
	}
	if feature.Value == DeliveredState && !maintenance {
		report, _, err := LatestFinalReport(repository, stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read final review"}
		}
		review, _, approval, err := DeliveryDocuments(repository, stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
		}
		var merge *DeliveryMerge
		if m, merged, err := ReadDeliveryMerge(repository, stream); err != nil {
			return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
		} else if merged {
			merge = &m
		}
		return DeliveryPresentation{Project: project, Workstream: stream, Report: report, ReviewRevision: review.Revision, Approval: approval, Publication: publication, Merge: merge, Delivered: true}, nil
	}
	if feature.Value != AssembledState && !maintenance {
		return DeliveryPresentation{}, &APIError{Code: Conflict, Message: "the workstream is not assembled"}
	}
	if wait, err := s.EdgePublicationWait(repository, stream); err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read dependency references"}
	} else if wait != "" {
		return DeliveryPresentation{}, &APIError{Code: Conflict, Message: wait}
	}
	report, reason, err := (&FinalReviewer{S: s, Repository: repository}).FinalGate(ctx, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read final review"}
	}
	if reason != "" {
		return DeliveryPresentation{}, &APIError{Code: Conflict, Message: reason}
	}
	review, _, approval, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
	}
	if review.Revision == 0 {
		return DeliveryPresentation{}, &APIError{Code: Conflict, Message: "no final review is recorded"}
	}
	slices.SortStableFunc(report.Criteria, func(a, b FinalCriterion) int {
		if (a.Gap != "") == (b.Gap != "") {
			return 0
		}
		if a.Gap != "" {
			return -1
		}
		return 1
	})
	title, draft, err := s.DraftDelivery(s.About(repository), repository, stream, report)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot draft the title and description from trace records"}
	}
	if approval != nil && (approval.Style != s.About(repository).Project.Landing || len(approval.Messages) == 0 || approval.Review != report.Review || approval.ReviewRevision != review.Revision || approval.Commit != report.Commit || approval.SpecHash != report.SpecHash || approval.Spec != report.Spec || approval.Plan != report.Plan || approval.Charter != report.Charter || approval.Seal != report.Seal || PublishedTitle(*approval, stream) != title) {
		approval = nil
	}
	style := s.About(repository).Project.Landing
	messages, err := DeliveryMessages(ctx, s.About(repository), report, style)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Code: Internal, Message: "cannot read delivery commit messages"}
	}
	pinned, _ := json.Marshal(struct {
		Title       string
		Description string
		Style       string
		Messages    []workspace.DeliveryMessage
	}{title, draft, style, messages})
	return DeliveryPresentation{Project: project, Workstream: stream, Report: report, ReviewRevision: review.Revision, Title: title, Draft: draft, DraftHash: DescriptionHash(string(pinned)), Messages: messages, Style: style, Approval: approval, Publication: publication, Delivered: feature.Value == DeliveredState, Maintenance: maintenance}, nil
}

func (s *Controller) ApproveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	presented, api := s.DeliveryPresentation(ctx, raw)
	if api != nil {
		return DeliveryApproval{}, api
	}
	if presented.Delivered && !presented.Maintenance {
		return DeliveryApproval{}, &APIError{Code: Conflict, Message: "the workstream is delivered"}
	}
	if req.Review != presented.Report.Review || req.ReviewRevision != presented.ReviewRevision || req.Commit != presented.Report.Commit || req.DraftHash != presented.DraftHash {
		return DeliveryApproval{}, &APIError{Code: Conflict, Message: "the final report or draft description changed; read the current presentation"}
	}
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return DeliveryApproval{}, api
	}
	if _, reason, err := (&FinalReviewer{S: s, Repository: repository}).FinalGate(ctx, stream); err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot validate final review"}
	} else if reason != "" {
		return DeliveryApproval{}, &APIError{Code: Conflict, Message: reason}
	}
	blocking := ReportBlocking(presented.Report)
	if len(blocking) > 0 && !req.AcceptBlocking {
		return DeliveryApproval{}, &APIError{Code: Conflict, Message: fmt.Sprintf("final review %d has blocking findings: %s; approve with accept_blocking to accept them as they are, or wait for the work that resolves them", presented.Report.Review, strings.Join(blocking, "; "))}
	}
	description := presented.Draft
	if presented.Approval != nil {
		description = presented.Approval.Description
	}
	if req.Description != nil {
		description = *req.Description
	}
	if strings.TrimSpace(description) == "" {
		return DeliveryApproval{}, &APIError{Code: Validation, Message: "description must not be empty"}
	}
	references, err := s.PendingMergeOrderReferences(repository, stream)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read dependency references"}
	}
	for _, reference := range references {
		if !strings.Contains(description, reference) {
			return DeliveryApproval{}, &APIError{Code: Validation, Message: "description must state the dependency on " + reference}
		}
	}
	messages := slices.Clone(presented.Messages)
	if presented.Approval != nil {
		messages = slices.Clone(presented.Approval.Messages)
	}
	if req.Messages != nil {
		if len(req.Messages) != len(messages) {
			return DeliveryApproval{}, &APIError{Code: Validation, Message: "supply every delivery commit message"}
		}
		for i, m := range req.Messages {
			if m.Commit != messages[i].Commit || strings.TrimSpace(m.Message) == "" || strings.ContainsRune(m.Message, 0) {
				return DeliveryApproval{}, &APIError{Code: Validation, Message: "delivery messages must name the presented commits and contain nonempty text"}
			}
			messages[i].Message = m.Message
		}
	}
	review, prior, existing, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
	}
	// Approving again after a refused publication records a new approval,
	// which asks for another publication.
	if existing != nil {
		refused, err := PublicationRefused(repository, stream, prior.Revision)
		if err != nil {
			return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read publication records"}
		}
		if refused {
			existing = nil
		}
	}
	if existing != nil && req.Description == nil && req.Messages == nil && existing.Style == presented.Style && existing.ReviewRevision == review.Revision && existing.Commit == req.Commit && existing.DraftHash == req.DraftHash {
		return *existing, nil
	}
	if existing != nil && existing.ReviewRevision == review.Revision && existing.Commit == req.Commit && existing.Style == presented.Style && slices.Equal(existing.Messages, messages) && existing.Description == description && existing.DraftHash == req.DraftHash {
		return *existing, nil
	}
	r := presented.Report
	approval := DeliveryApproval{Review: r.Review, ReviewRevision: review.Revision, Commit: r.Commit, Seal: r.Seal, SpecHash: r.SpecHash, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter, DraftHash: req.DraftHash, Title: presented.Title, Description: description, DescriptionHash: DescriptionHash(description), Messages: messages, Style: presented.Style, At: s.Now(), AcceptedBlocking: blocking}
	content, err := json.Marshal(approval)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot encode delivery approval"}
	}
	state, err := repository.Workflow(stream, DeliverySubject)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read delivery state"}
	}
	id := fmt.Sprintf("delivery-owner-%d", prior.Revision+1)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DeliveryDocument, Revision: prior.Revision + 1, Project: project, Workstream: stream, At: approval.At, Actor: ownerActor, Cause: id}, Path: DeliveryPath, Content: string(content)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: ownerHeader(id, project, stream, "delivery-approval", approval.At), Subject: DeliverySubject, From: state.Value, To: fmt.Sprintf("approved-%d", prior.Revision+1), Reason: fmt.Sprintf("the owner approved description %s for final review %d at %s", approval.DescriptionHash, approval.Review, approval.Commit)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return DeliveryApproval{}, &APIError{Code: Conflict, Message: "delivery decision changed; read the current presentation"}
		}
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot record delivery approval"}
	}
	return approval, nil
}

// deliveryGate checks the approval against the current review and the exact
// description publication proposes to send.
func (s *Controller) DeliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (DeliveryApproval, string, error) {
	if wait, err := s.EdgePublicationWait(repository, stream); err != nil || wait != "" {
		return DeliveryApproval{}, wait, err
	}
	references, err := s.PendingMergeOrderReferences(repository, stream)
	if err != nil {
		return DeliveryApproval{}, "", err
	}
	for _, reference := range references {
		if !strings.Contains(description, reference) {
			return DeliveryApproval{}, "the approved description must state the dependency on " + reference, nil
		}
	}
	if pending, err := s.UnresolvedWork(repository, stream); err != nil {
		return DeliveryApproval{}, "", err
	} else if pending {
		return DeliveryApproval{}, "necessary discovered work or a plan revision remains unresolved", nil
	}

	report, reason, err := (&FinalReviewer{S: s, Repository: repository}).FinalGate(ctx, stream)
	if err != nil || reason != "" {
		return DeliveryApproval{}, reason, err
	}
	review, _, approval, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, "", err
	}
	if approval == nil {
		return DeliveryApproval{}, "no owner delivery approval is recorded", nil
	}
	if approval.Review != report.Review || approval.ReviewRevision != review.Revision || approval.Commit != report.Commit || approval.Seal != report.Seal || approval.SpecHash != report.SpecHash || approval.Spec != report.Spec || approval.Plan != report.Plan || approval.Charter != report.Charter {
		return *approval, "the owner approval is stale; review and approve the current branch", nil
	}
	if approval.Style != s.About(repository).Project.Landing || len(approval.Messages) == 0 {
		return *approval, "delivery messages and style require current owner approval", nil
	}
	if approval.DescriptionHash != DescriptionHash(approval.Description) || approval.Description != description {
		return *approval, "the description differs from what the owner approved", nil
	}
	if blocking := ReportBlocking(report); !slices.Equal(blocking, approval.AcceptedBlocking) {
		return *approval, fmt.Sprintf("final review %d found blocking problems the owner approval does not accept", report.Review), nil
	}
	return *approval, "", nil
}

func DeliveryMessages(ctx context.Context, cfg *config.Config, report FinalReport, style string) ([]workspace.DeliveryMessage, error) {
	if report.Upstream == nil {
		return nil, errors.New("final review has no base")
	}
	if style == SquashStyle {
		message := strings.TrimSpace(report.Summary)
		if message == "" {
			message = "Deliver feature"
		}
		return []workspace.DeliveryMessage{{Commit: report.Commit, Message: message}}, nil
	}
	return (&workspace.Git{Clone: cfg.Project.Clone}).DeliveryMessages(ctx, report.Upstream.Commit, report.Commit)
}
