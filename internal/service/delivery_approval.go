package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

const deliveryPath = "final/delivery.json"
const deliveryDocument = "final-delivery"
const deliverySubject = "delivery-owner"

// DeliveryPresentation is the final review and the title and description
// presented to the owner. Criteria with a gap precede criteria with
// evidence. A delivered workstream presents the report, approval and
// publication it was delivered with, and no draft.
type DeliveryPresentation struct {
	Project        config.ProjectID            `json:"project"`
	Workstream     config.WorkstreamID         `json:"workstream"`
	Report         FinalReport                 `json:"report"`
	ReviewRevision int                         `json:"review_revision"`
	Title          string                      `json:"title,omitempty"`
	Draft          string                      `json:"draft"`
	DraftHash      string                      `json:"draft_hash"`
	Messages       []workspace.DeliveryMessage `json:"messages"`
	Style          string                      `json:"style"`
	Approval       *DeliveryApproval           `json:"approval,omitempty"`
	// Publication is the latest record of publishing an approval, Merge the
	// owner's own merge of the branch into upstream that delivered it
	// instead, and Delivered whether the workstream is delivered.
	Publication *DeliveryPublication `json:"publication,omitempty"`
	Merge       *DeliveryMerge       `json:"merge,omitempty"`
	Delivered   bool                 `json:"delivered,omitempty"`
	Maintenance bool                 `json:"maintenance,omitempty"`
}

// DeliveryApproval preserves the exact title and text and reviewed identity
// the owner approved. Title is empty in an approval recorded before the
// title became part of the draft; deliveryTitle(Description, stream) is
// then used instead, as it was when that approval was recorded.
// DescriptionHash allows publication to check the text it will send.
type DeliveryApproval struct {
	Review          int                         `json:"review"`
	ReviewRevision  int                         `json:"review_revision"`
	Commit          string                      `json:"commit"`
	Seal            int                         `json:"seal"`
	SpecHash        string                      `json:"spec_hash"`
	Spec            int                         `json:"spec"`
	Plan            int                         `json:"plan"`
	Charter         int                         `json:"charter"`
	DraftHash       string                      `json:"draft_hash"`
	Title           string                      `json:"title,omitempty"`
	Description     string                      `json:"description"`
	DescriptionHash string                      `json:"description_hash"`
	At              time.Time                   `json:"at"`
	Messages        []workspace.DeliveryMessage `json:"messages"`
	Style           string                      `json:"style"`
}

// publishedTitle is the title to publish for an approval: the title it
// recorded, or, for an approval recorded before this field existed,
// deliveryTitle(Description, stream), which is what was published for it
// then.
func publishedTitle(approval DeliveryApproval, stream config.WorkstreamID) string {
	if approval.Title != "" {
		return approval.Title
	}
	return deliveryTitle(approval.Description, stream)
}

// DeliveryDecision approves the draft as shown, or supplies the owner's edit.
// The review identity and draft hash are required even when the text is edited.
type DeliveryDecision struct {
	Review         int                         `json:"review"`
	ReviewRevision int                         `json:"review_revision"`
	Commit         string                      `json:"commit"`
	DraftHash      string                      `json:"draft_hash"`
	Description    *string                     `json:"description,omitempty"`
	Messages       []workspace.DeliveryMessage `json:"messages,omitempty"`
}

func descriptionHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

// boundSubject truncates s to subjectLength runes, as deliveryTitle and a
// landing commit subject do.
func boundSubject(s string) string {
	if runes := []rune(s); len(runes) > subjectLength {
		return string(append(runes[:subjectLength-1], '…'))
	}
	return s
}

func deliveryDocuments(repository *trace.Repository, stream config.WorkstreamID) (trace.Document, trace.Document, *DeliveryApproval, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return trace.Document{}, trace.Document{}, nil, err
	}
	var review, decision trace.Document
	for _, d := range docs {
		switch d.ID {
		case finalReportDocument:
			review = d
		case deliveryDocument:
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

// headingLine matches a Markdown heading line, capturing its text.
var headingLine = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)[ \t]*$`)

// firstHeading returns the text of the first Markdown heading in content, or
// "" if content holds none.
func firstHeading(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if m := headingLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			if title := strings.TrimSpace(m[1]); title != "" {
				return title
			}
		}
	}
	return ""
}

// summaryStructural matches a Markdown line that specSummary excludes from
// the prose it extracts: a heading, a bullet or numbered list item, a code
// fence or an HTML comment.
var summaryStructural = regexp.MustCompile(`^ {0,3}(#{1,6}[ \t]|[-*+][ \t]|[0-9]{1,9}[.)][ \t]|` + "```" + `|~~~|<!--)`)

// specSummaryBound is the longest specSummary returns, in runes.
const specSummaryBound = 600

// specSummary returns a short prose summary of the sealed spec: its first
// paragraph of plain text, skipping headings, list items, fenced code and
// HTML comments, bounded to specSummaryBound runes. It is deterministic:
// presenting the same spec revision twice yields the same summary.
func specSummary(content string) string {
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
		if summaryStructural.MatchString(line) {
			continue
		}
		paragraph = append(paragraph, trimmed)
	}
	text := strings.Join(paragraph, " ")
	if runes := []rune(text); len(runes) > specSummaryBound {
		text = string(runes[:specSummaryBound-1]) + "…"
	}
	return text
}

// workstreamTitle is the title shown to the owner and used as the pull
// request title: the chief of staff's latest status goal, the value
// archiveTitle and status also use; when none is recorded, a fallback
// derived from recorded documents (the sealed spec's first heading, or the
// handed issue's title) before the bare workstream ID. It is bounded to the
// same subject length as a landing commit.
func workstreamTitle(repository *trace.Repository, stream config.WorkstreamID, spec, handed trace.Document) (string, error) {
	statuses, err := repository.Statuses()
	if err != nil {
		return "", err
	}
	for _, w := range statuses {
		if w.Workstream == stream && w.Status != nil && w.Status.Goal != "" {
			return boundSubject(w.Status.Goal), nil
		}
	}
	if title := firstHeading(spec.Content); title != "" {
		return boundSubject(title), nil
	}
	if title := firstHeading(handed.Content); title != "" {
		return boundSubject(title), nil
	}
	return "Workstream " + string(stream), nil
}

// closingReference is a GitHub closing reference to the issue handed
// recorded as its source: "closes #N" when the issue is in targetRepo, or
// "closes OWNER/REPO#N" otherwise. It is "" for a workstream handed in from
// a file or stdin, or with no handed document recorded.
func closingReference(handed trace.Document, targetRepo string) string {
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
// (see workstreamTitle) and a short prose summary of the sealed spec,
// closing the issue the workstream was handed in from when one is recorded
// and its repository can be compared with the one the pull request would
// target, and the workstream's logged assumptions, when it has any. All of
// it comes from documents already in the trace, so presenting the same
// recorded trace twice produces the same draft, and no agent is called.
func draftDelivery(cfg *config.Config, repository *trace.Repository, stream config.WorkstreamID, report FinalReport) (title, body string, err error) {
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
	title, err = workstreamTitle(repository, stream, spec, handed)
	if err != nil {
		return "", "", err
	}
	summary := specSummary(spec.Content)
	if summary == "" {
		summary = title
	}
	targetRepo := cfg.Project.Upstream
	if report.Upstream != nil && report.Upstream.Workstream != "" {
		targetRepo = cfg.Project.PushRepository()
	}
	body = summary + "\n"
	if ref := closingReference(handed, targetRepo); ref != "" {
		body += "\n" + ref + "\n"
	}
	// A workgroup merge-order edge whose upstream has not yet merged keeps
	// the pull request a draft; the owner sees the dependency stated here,
	// as part of the one delivery approval, so marking it ready later needs
	// no separate owner prompt (spec#12).
	if references, err := pendingMergeOrderReferences(repository, stream); err != nil {
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

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return DeliveryPresentation{}, api
	}
	return s.presentDelivery(ctx, project, stream, repository)
}

// presentDelivery builds the presentation of one workstream of the project.
func (s *Service) presentDelivery(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository) (DeliveryPresentation, *APIError) {
	if pending, err := unresolvedWork(repository, stream); err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read discovered work"}
	} else if pending {
		return DeliveryPresentation{}, &APIError{Conflict, "necessary discovered work or a plan revision remains unresolved"}
	}

	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read feature state"}
	}
	published, err := publications(repository, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read publication records"}
	}
	var publication *DeliveryPublication
	if len(published) > 0 {
		publication = &published[len(published)-1]
	}
	maintenance := false
	if feature.Value == DeliveredState {
		maintenance, err = upstreamDeliveryPending(repository, s.about(repository), stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Internal, "cannot read dependency integration"}
		}
	}
	if feature.Value == DeliveredState && !maintenance {
		report, _, err := latestFinalReport(repository, stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Internal, "cannot read final review"}
		}
		review, _, approval, err := deliveryDocuments(repository, stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Internal, "cannot read delivery records"}
		}
		var merge *DeliveryMerge
		if m, merged, err := deliveryMerge(repository, stream); err != nil {
			return DeliveryPresentation{}, &APIError{Internal, "cannot read delivery records"}
		} else if merged {
			merge = &m
		}
		return DeliveryPresentation{Project: project, Workstream: stream, Report: report, ReviewRevision: review.Revision, Approval: approval, Publication: publication, Merge: merge, Delivered: true}, nil
	}
	if feature.Value != AssembledState && !maintenance {
		return DeliveryPresentation{}, &APIError{Conflict, "the workstream is not assembled"}
	}
	if wait, err := edgePublicationWait(repository, stream); err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read dependency references"}
	} else if wait != "" {
		return DeliveryPresentation{}, &APIError{Conflict, wait}
	}
	report, reason, err := (&finalReviewer{s: s, repository: repository}).finalGate(ctx, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read final review"}
	}
	// An unshown criterion is part of the report the owner must see, even
	// though that report cannot authorize delivery yet.
	if reason != "" && !strings.Contains(reason, "unresolved gap") {
		return DeliveryPresentation{}, &APIError{Conflict, reason}
	}
	review, _, approval, err := deliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read delivery records"}
	}
	if review.Revision == 0 {
		return DeliveryPresentation{}, &APIError{Conflict, "no final review is recorded"}
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
	title, draft, err := draftDelivery(s.about(repository), repository, stream, report)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot draft the title and description from trace records"}
	}
	if approval != nil && (approval.Style != s.about(repository).Project.Landing || len(approval.Messages) == 0 || approval.Review != report.Review || approval.ReviewRevision != review.Revision || approval.Commit != report.Commit || approval.SpecHash != report.SpecHash || approval.Spec != report.Spec || approval.Plan != report.Plan || approval.Charter != report.Charter || approval.Seal != report.Seal || publishedTitle(*approval, stream) != title) {
		approval = nil
	}
	style := s.about(repository).Project.Landing
	messages, err := deliveryMessages(ctx, s.about(repository), report, style)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read delivery commit messages"}
	}
	pinned, _ := json.Marshal(struct {
		Title       string
		Description string
		Style       string
		Messages    []workspace.DeliveryMessage
	}{title, draft, style, messages})
	return DeliveryPresentation{Project: project, Workstream: stream, Report: report, ReviewRevision: review.Revision, Title: title, Draft: draft, DraftHash: descriptionHash(string(pinned)), Messages: messages, Style: style, Approval: approval, Publication: publication, Delivered: feature.Value == DeliveredState, Maintenance: maintenance}, nil
}

func (s *Service) approveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	presented, api := s.deliveryPresentation(ctx, raw)
	if api != nil {
		return DeliveryApproval{}, api
	}
	if presented.Delivered && !presented.Maintenance {
		return DeliveryApproval{}, &APIError{Conflict, "the workstream is delivered"}
	}
	if req.Review != presented.Report.Review || req.ReviewRevision != presented.ReviewRevision || req.Commit != presented.Report.Commit || req.DraftHash != presented.DraftHash {
		return DeliveryApproval{}, &APIError{Conflict, "the final report or draft description changed; read the current presentation"}
	}
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return DeliveryApproval{}, api
	}
	if _, reason, err := (&finalReviewer{s: s, repository: repository}).finalGate(ctx, stream); err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot validate final review"}
	} else if reason != "" {
		return DeliveryApproval{}, &APIError{Conflict, reason}
	}
	description := presented.Draft
	if presented.Approval != nil {
		description = presented.Approval.Description
	}
	if req.Description != nil {
		description = *req.Description
	}
	if strings.TrimSpace(description) == "" {
		return DeliveryApproval{}, &APIError{Validation, "description must not be empty"}
	}
	references, err := pendingMergeOrderReferences(repository, stream)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot read dependency references"}
	}
	for _, reference := range references {
		if !strings.Contains(description, reference) {
			return DeliveryApproval{}, &APIError{Validation, "description must state the dependency on " + reference}
		}
	}
	messages := slices.Clone(presented.Messages)
	if presented.Approval != nil {
		messages = slices.Clone(presented.Approval.Messages)
	}
	if req.Messages != nil {
		if len(req.Messages) != len(messages) {
			return DeliveryApproval{}, &APIError{Validation, "supply every delivery commit message"}
		}
		for i, m := range req.Messages {
			if m.Commit != messages[i].Commit || strings.TrimSpace(m.Message) == "" || strings.ContainsRune(m.Message, 0) {
				return DeliveryApproval{}, &APIError{Validation, "delivery messages must name the presented commits and contain nonempty text"}
			}
			messages[i].Message = m.Message
		}
	}
	review, prior, existing, err := deliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot read delivery records"}
	}
	// Approving again after a refused publication records a new approval,
	// which asks for another publication.
	if existing != nil {
		refused, err := publicationRefused(repository, stream, prior.Revision)
		if err != nil {
			return DeliveryApproval{}, &APIError{Internal, "cannot read publication records"}
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
	approval := DeliveryApproval{Review: r.Review, ReviewRevision: review.Revision, Commit: r.Commit, Seal: r.Seal, SpecHash: r.SpecHash, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter, DraftHash: req.DraftHash, Title: presented.Title, Description: description, DescriptionHash: descriptionHash(description), Messages: messages, Style: presented.Style, At: s.now()}
	content, err := json.Marshal(approval)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot encode delivery approval"}
	}
	state, err := repository.Workflow(stream, deliverySubject)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot read delivery state"}
	}
	id := fmt.Sprintf("delivery-owner-%d", prior.Revision+1)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: deliveryDocument, Revision: prior.Revision + 1, Project: project, Workstream: stream, At: approval.At, Actor: ownerActor, Cause: id}, Path: deliveryPath, Content: string(content)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: ownerHeader(id, project, stream, "delivery-approval", approval.At), Subject: deliverySubject, From: state.Value, To: fmt.Sprintf("approved-%d", prior.Revision+1), Reason: fmt.Sprintf("the owner approved description %s for final review %d at %s", approval.DescriptionHash, approval.Review, approval.Commit)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return DeliveryApproval{}, &APIError{Conflict, "delivery decision changed; read the current presentation"}
		}
		return DeliveryApproval{}, &APIError{Internal, "cannot record delivery approval"}
	}
	return approval, nil
}

// deliveryGate checks the approval against the current review and the exact
// description publication proposes to send.
func (s *Service) deliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (DeliveryApproval, string, error) {
	if wait, err := edgePublicationWait(repository, stream); err != nil || wait != "" {
		return DeliveryApproval{}, wait, err
	}
	references, err := pendingMergeOrderReferences(repository, stream)
	if err != nil {
		return DeliveryApproval{}, "", err
	}
	for _, reference := range references {
		if !strings.Contains(description, reference) {
			return DeliveryApproval{}, "the approved description must state the dependency on " + reference, nil
		}
	}
	if pending, err := unresolvedWork(repository, stream); err != nil {
		return DeliveryApproval{}, "", err
	} else if pending {
		return DeliveryApproval{}, "necessary discovered work or a plan revision remains unresolved", nil
	}

	report, reason, err := (&finalReviewer{s: s, repository: repository}).finalGate(ctx, stream)
	if err != nil || reason != "" {
		return DeliveryApproval{}, reason, err
	}
	review, _, approval, err := deliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, "", err
	}
	if approval == nil {
		return DeliveryApproval{}, "no owner delivery approval is recorded", nil
	}
	if approval.Review != report.Review || approval.ReviewRevision != review.Revision || approval.Commit != report.Commit || approval.Seal != report.Seal || approval.SpecHash != report.SpecHash || approval.Spec != report.Spec || approval.Plan != report.Plan || approval.Charter != report.Charter {
		return *approval, "the owner approval is stale; review and approve the current branch", nil
	}
	if approval.Style != s.about(repository).Project.Landing || len(approval.Messages) == 0 {
		return *approval, "delivery messages and style require current owner approval", nil
	}
	if approval.DescriptionHash != descriptionHash(approval.Description) || approval.Description != description {
		return *approval, "the description differs from what the owner approved", nil
	}
	return *approval, "", nil
}

func deliveryMessages(ctx context.Context, cfg *config.Config, report FinalReport, style string) ([]workspace.DeliveryMessage, error) {
	if report.Upstream == nil {
		return nil, errors.New("final review has no base")
	}
	if style == squashStyle {
		message := strings.TrimSpace(report.Summary)
		if message == "" {
			message = "Deliver feature"
		}
		return []workspace.DeliveryMessage{{Commit: report.Commit, Message: message}}, nil
	}
	return (&workspace.Git{Clone: cfg.Project.Clone}).DeliveryMessages(ctx, report.Upstream.Commit, report.Commit)
}
