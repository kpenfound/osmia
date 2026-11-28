package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/trace"
)

const briefToolName = delivery.BriefToolName

func (c *runtimeControls) deliveryBrief(repo *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: briefToolName, Effect: coreadapter.ToolMemory,
		Description: fmt.Sprintf("Record the delivery brief the owner reads before deciding delivery of a workstream's final review. Name the review with review and review_revision exactly as the final review notice gives them; a call naming any other review, a review the owner already approved, or a workstream with no reviewed final review awaiting approval is refused. Set workstream to the member (required for a shared manager). text: how the approved intent shows up in the product, then at most one line on how it was implemented, then any departure from the intent, at most %d characters; never list tests, files or commits. before_you_ship: optional lines, one per item, at most %d characters in total, only for what must be done to deploy the change. screenshots: at most %d image_id values returned by preview for this member at the reviewed commit. A refused call records nothing and says why; nothing is ever truncated. A later accepted call for the same review replaces the earlier brief.", delivery.MaxBriefText, delivery.MaxBriefItems, delivery.MaxBriefScreenshots),
		InputSchema: memberSchema(scope, `"review":{"type":"integer","minimum":1},"review_revision":{"type":"integer","minimum":1},"text":{"type":"string"},"before_you_ship":{"type":"array","items":{"type":"string"}},"screenshots":{"type":"array","items":{"type":"string"}}`)}
	var schema map[string]any
	_ = json.Unmarshal(tool.InputSchema, &schema)
	schema["required"] = []string{"review", "review_revision", "text"}
	tool.InputSchema, _ = json.Marshal(schema)
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if err := repo.ActiveTurn(scope); err != nil {
			return nil, err
		}
		refuse := func(format string, args ...any) (json.RawMessage, error) {
			return json.Marshal(struct {
				Recorded bool   `json:"recorded"`
				Reason   string `json:"reason"`
			}{false, fmt.Sprintf(format, args...)})
		}
		var in struct {
			Review         int                 `json:"review"`
			ReviewRevision int                 `json:"review_revision"`
			Text           string              `json:"text"`
			BeforeYouShip  []string            `json:"before_you_ship"`
			Screenshots    []string            `json:"screenshots"`
			Workstream     config.WorkstreamID `json:"workstream"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		if scope.Role != trace.Manager {
			return refuse("%s is only available to the manager", briefToolName)
		}
		own := config.WorkstreamID(scope.Workstream)
		members, err := repo.WorkgroupMembers(own)
		if err != nil {
			return nil, err
		}
		if len(members) > 1 && in.Workstream == "" {
			return refuse("a shared manager must name the member workstream the brief is for")
		}
		target, stream, refusal, err := managerMember(repo, scope, in.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return refuse("%s", refusal)
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("the delivery brief is unavailable")
		}
		brief, revision, reason, err := s.delivery().RecordBrief(ctx, target, stream, delivery.BriefInput{Review: in.Review, ReviewRevision: in.ReviewRevision, Text: in.Text, BeforeYouShip: in.BeforeYouShip, Screenshots: in.Screenshots, Turn: scope.Turn}, previewScreenshots(repo, own, target, stream))
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return refuse("%s", reason)
		}
		return json.Marshal(struct {
			Recorded       bool   `json:"recorded"`
			Review         int    `json:"review"`
			ReviewRevision int    `json:"review_revision"`
			Commit         string `json:"commit"`
			Revision       int    `json:"revision"`
			Next           string `json:"next"`
		}{true, brief.Review, brief.ReviewRevision, brief.Commit, revision, fmt.Sprintf("Recorded for review %d. Call %s again for the same review to replace it until the owner approves delivery.", brief.Review, briefToolName)})
	}
	return tool
}

func previewScreenshots(repo *trace.Repository, own config.WorkstreamID, target *trace.Repository, stream config.WorkstreamID) delivery.ScreenshotCheck {
	return func(commit string, screenshots []string) (string, error) {
		docs, err := trace.Read[trace.Document](repo, own)
		if err != nil {
			return "", err
		}
		latest := map[string]trace.Document{}
		for _, doc := range docs {
			if strings.HasPrefix(doc.ID, "preview-") && doc.Revision > latest[doc.ID].Revision {
				latest[doc.ID] = doc
			}
		}
		captured := map[string]bool{}
		for _, doc := range latest {
			var op previewOperation
			if json.Unmarshal([]byte(doc.Content), &op) != nil || op.Result == nil {
				continue
			}
			if op.Workstream == stream && op.Commit == commit && op.Result.ImageID != "" && op.Result.Limitation == "" {
				captured[op.Result.ImageID] = true
			}
		}
		for _, id := range screenshots {
			if !captured[id] {
				return fmt.Sprintf("screenshot %q is not an image preview captured of %s at the reviewed commit %s; take it with preview and pass its image_id", id, stream, commit), nil
			}
			if _, err := target.StoredImage(stream, id); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Sprintf("screenshot %q is not stored for %s", id, stream), nil
				}
				return "", err
			}
		}
		return "", nil
	}
}

func (s *Service) managerThread(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (trace.Thread, bool, error) {
	if s.options.Threads == nil {
		return trace.Thread{}, false, nil
	}
	target, _, _, closeTarget, err := s.managerRepositoryForRead(ctx, repository, stream)
	if closeTarget != nil {
		defer closeTarget()
	}
	switch {
	case errors.Is(err, errWorkgroupJoining), errors.Is(err, trace.ErrLocked), errors.Is(err, trace.ErrMembershipChanged):
		return trace.Thread{}, true, nil
	case errors.Is(err, trace.ErrMemberRetired):
		return trace.Thread{}, false, nil
	case err != nil:
		return trace.Thread{}, false, err
	}
	t, err := target.ManagerThread(stream)
	if errors.Is(err, os.ErrNotExist) {
		return trace.Thread{}, false, nil
	}
	return t, err == nil, err
}

const (
	checksTool = delivery.ChecksToolName
)

func (c *runtimeControls) deliveryController() *delivery.Controller {
	if s := c.service.Load(); s != nil {
		return s.delivery()
	}
	return nil
}

const triageTool = delivery.TriageToolName

func (c *runtimeControls) triageFinding(repo *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return delivery.TriageToolOf(func() *delivery.Controller {
		if s := c.service.Load(); s != nil {
			return s.delivery()
		}
		return nil
	}, repo, scope)
}
