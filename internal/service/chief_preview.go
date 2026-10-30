package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

type previewInput struct {
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Unit       string              `json:"unit,omitempty"`
	Path       string              `json:"path,omitempty"`
	Viewport   string              `json:"viewport,omitempty"`
	FullPage   bool                `json:"full_page,omitempty"`
}

type previewResult struct {
	ImageID    string              `json:"image_id,omitempty"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Commit     string              `json:"commit,omitempty"`
	Path       string              `json:"path,omitempty"`
	Limitation string              `json:"limitation,omitempty"`
}

// previewOperation pins the exact target and configuration before any capture.
// The member image is the receipt for the external effect; Result follows the
// shared reference and view copy, which are independently reconciled.
type previewOperation struct {
	Scope      coreadapter.Scope     `json:"scope"`
	Call       json.RawMessage       `json:"call"`
	Workstream config.WorkstreamID   `json:"workstream"`
	Commit     string                `json:"commit"`
	Preview    config.Preview        `json:"preview"`
	Capture    checks.PreviewCapture `json:"capture"`
	Result     *previewResult        `json:"result,omitempty"`
}

func (c *runtimeControls) preview(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, view *isolation.FileView, now func() time.Time) coreadapter.Tool {
	var mu sync.Mutex
	tool := coreadapter.Tool{Name: "preview", Effect: coreadapter.ToolMemory,
		Description: "Ask the service to capture a static screenshot of built code. Set workstream to the member to show (required for a shared chief). Defaults to its feature-branch head, or set unit for its recorded candidate. path defaults to the project's preview path; viewport is desktop (default) or phone, full_page defaults to false. Returns an image_id to refer to in your reply and a copy under images/ in this read-only view. Limitations do not fail your turn. Recovered results from an interrupted turn are in previews/results.json; read them before requesting another capture.",
		InputSchema: memberSchema(scope, `"unit":{"type":"string"},"path":{"type":"string"},"viewport":{"type":"string","enum":["desktop","phone"]},"full_page":{"type":"boolean"}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		limitation := func(message string) (json.RawMessage, error) {
			return json.Marshal(previewResult{Limitation: previewDiagnostic(message)})
		}
		if scope.Role != trace.ChiefOfStaff {
			return limitation("preview is only available to the chief of staff")
		}
		var in previewInput
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			return limitation("invalid preview arguments")
		}
		if in.Viewport != "" && in.Viewport != "desktop" && in.Viewport != "phone" {
			return limitation("viewport must be desktop or phone")
		}
		if in.Path != "" && (!strings.HasPrefix(in.Path, "/") || strings.ContainsAny(in.Path, "\r\n\x00")) {
			return limitation("preview path must start with / and contain no control characters")
		}
		key := trace.ToolCallID(scope, "preview", raw)
		id := "preview-" + strings.TrimPrefix(key, "tool-")
		own := config.WorkstreamID(scope.Workstream)
		doc, err := latestPreviewDocument(repo, own, id)
		if err != nil {
			return limitation(err.Error())
		}
		var op previewOperation
		if doc.ID == "" {
			if err := repo.ActiveTurn(scope); err != nil {
				return nil, err
			}
			if _, err := repo.BeginTool(ctx, scope, tool, raw, now); err != nil {
				return nil, err
			}
			members, err := repo.WorkgroupMembers(own)
			if err != nil {
				return limitation(err.Error())
			}
			if len(members) > 1 && in.Workstream == "" {
				return limitation("a shared chief must qualify workstream for preview")
			}
			target, stream, refusal, err := chiefMember(repo, scope, in.Workstream)
			if err != nil {
				return limitation(err.Error())
			}
			if refusal != "" {
				return limitation(refusal)
			}
			project := cfg.For(target.Project())
			if project.Project.Preview == nil {
				return limitation("the project has no [preview] configured")
			}
			g, err := featureWorkspaces(project, target).Of(stream)
			if err != nil {
				return limitation("nothing is built yet: " + err.Error())
			}
			commit, exists, err := g.Branch(ctx, featureBranch(stream))
			if err != nil {
				return limitation(err.Error())
			}
			if in.Unit != "" {
				reportDoc, err := latestPreviewDocument(target, stream, reportDocument(in.Unit))
				if err != nil {
					return limitation(err.Error())
				}
				var report UnitReport
				if reportDoc.ID != "" {
					err = json.Unmarshal([]byte(reportDoc.Content), &report)
				}
				if err != nil || report.Candidate == "" {
					return limitation("unit " + in.Unit + " has no recorded candidate or cannot be resolved")
				}
				commit, exists = report.Candidate, true
			} else if exists {
				if sealed, _, found, err := seal.Latest(target, stream); err == nil && found && sealed.Base.Commit == commit {
					return limitation("nothing is built yet on the feature branch")
				}
			}
			if !exists || commit == "" {
				return limitation("nothing is built yet on the feature branch")
			}
			preview := *project.Project.Preview
			if in.Path == "" {
				in.Path = preview.Path
			}
			if in.Viewport == "" {
				in.Viewport = "desktop"
			}
			op = previewOperation{Scope: scope, Call: bytes.Clone(raw), Workstream: stream, Commit: commit, Preview: preview,
				Capture: checks.PreviewCapture{Call: preview.Call, Port: preview.Port, Path: in.Path, Viewport: in.Viewport, FullPage: in.FullPage}}
			doc = trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: own, At: now(), Actor: serviceActor, Cause: scope.Turn}, Path: "tools/" + id + ".json"}
			if err := writePreviewOperation(ctx, repo, &doc, op, now); err != nil {
				return limitation(err.Error())
			}
		} else if err := json.Unmarshal([]byte(doc.Content), &op); err != nil {
			return limitation(err.Error())
		}
		target, stream, refusal, err := chiefMember(repo, scope, op.Workstream)
		if err != nil {
			return limitation(err.Error())
		}
		if refusal != "" {
			return limitation(refusal)
		}
		image, err := target.ImageByKey(stream, key)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return limitation(err.Error())
		}
		if errors.Is(err, fs.ErrNotExist) {
			if op.Result != nil {
				output, _ := json.Marshal(op.Result)
				if err := finishPreviewTool(ctx, repo, scope, raw, output, now); err != nil {
					return limitation(err.Error())
				}
				return output, nil
			}
			service := c.service.Load()
			if service == nil {
				return limitation("preview service is unavailable")
			}
			data, runErr := service.capturePreview(ctx, cfg.For(target.Project()), target, stream, op)
			if runErr == nil {
				image, runErr = target.RecordImage(ctx, trace.Header{Project: target.Project(), Workstream: stream, At: now(), Actor: serviceActor, Cause: scope.Turn}, key, "image/png", data, scope.Turn)
			}
			if runErr != nil {
				result := previewResult{Limitation: previewDiagnostic(runErr.Error())}
				// A cancelled turn may be reclaimed; without an image it must capture again.
				if ctx.Err() == nil {
					op.Result = &result
					doc.Revision++
					if err := writePreviewOperation(ctx, repo, &doc, op, now); err != nil {
						return limitation(err.Error())
					}
				}
				output, _ := json.Marshal(result)
				if ctx.Err() == nil {
					if err := finishPreviewTool(ctx, repo, scope, raw, output, now); err != nil {
						return limitation(err.Error())
					}
				}
				return output, nil
			}
		}
		result, err := reconcilePreviewImage(ctx, repo, target, scope, view, op, image, now)
		if err != nil {
			return limitation(err.Error())
		}
		if op.Result == nil || *op.Result != result {
			op.Result = &result
			doc.Revision++
			if err := writePreviewOperation(ctx, repo, &doc, op, now); err != nil {
				return limitation(err.Error())
			}
		}
		output, _ := json.Marshal(result)
		// This also completes the original audit record when recovery happens before
		// the model repeats its call. BeginTool and its completion are idempotent.
		if err := finishPreviewTool(ctx, repo, scope, raw, output, now); err != nil {
			return limitation(err.Error())
		}
		return output, nil
	}
	return tool
}

func latestPreviewDocument(repo *trace.Repository, stream config.WorkstreamID, id string) (trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	var latest trace.Document
	for _, doc := range docs {
		if doc.ID == id && doc.Revision > latest.Revision {
			latest = doc
		}
	}
	return latest, err
}

func writePreviewOperation(ctx context.Context, repo *trace.Repository, doc *trace.Document, op previewOperation, now func() time.Time) error {
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	doc.At, doc.Content = now(), string(data)
	return repo.RecordDocuments(ctx, []trace.Document{*doc})
}

func (s *Service) capturePreview(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID, op previewOperation) ([]byte, error) {
	return s.withPreviewSlot(ctx, CheckSlotRun{Project: repo.Project(), Workstream: stream, Preview: true}, op.Preview.Timeout, func(ctx context.Context) ([]byte, error) {
		dir, err := os.MkdirTemp("", "osmia-preview-source-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		g, err := featureWorkspaces(cfg, repo).Of(stream)
		if err != nil {
			return nil, err
		}
		if err := g.Export(ctx, op.Commit, dir); err != nil {
			return nil, fmt.Errorf("preview source export: %w", err)
		}
		resolved := cfg.Dagger.Resolve()
		token := os.Getenv(resolved.CloudTokenEnv)
		redact.Register(token)
		selection := string(resolved.Mode)
		if resolved.Address != "" {
			selection = resolved.Address
		}
		return (checks.DaggerChecks{Engine: selection, CloudToken: token}).Preview(ctx, dir, op.Capture)
	})
}

func (s *Service) withPreviewSlot(ctx context.Context, run CheckSlotRun, timeout string, capture func(context.Context) ([]byte, error)) ([]byte, error) {
	release, err := s.checkSlots.acquire(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("preview cancelled while waiting: %w", err)
	}
	defer release()
	duration, err := time.ParseDuration(timeout)
	if err != nil || duration <= 0 {
		return nil, errors.New("preview timeout is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	return capture(ctx)
}

func reconcilePreviewImage(ctx context.Context, repo, target *trace.Repository, scope coreadapter.Scope, view *isolation.FileView, op previewOperation, image trace.Image, now func() time.Time) (previewResult, error) {
	own := config.WorkstreamID(scope.Workstream)
	members, err := repo.WorkgroupMembers(own)
	if err != nil {
		return previewResult{}, err
	}
	if target != repo || len(members) > 1 {
		_, err := repo.PublishImageReference(ctx, trace.Header{Project: repo.Project(), Workstream: own, At: now(), Actor: serviceActor, Cause: scope.Turn}, op.Workstream, image.ID, scope.Turn)
		if err != nil {
			return previewResult{}, err
		}
	}
	// Only the service writes this read-only view. The name is derived from a
	// stored image ID; the model retains no write or execution capability.
	root, err := os.OpenRoot(view.Workspace().Directory)
	if err != nil {
		return previewResult{}, err
	}
	defer root.Close()
	if err := root.MkdirAll("images", 0700); err != nil {
		return previewResult{}, err
	}
	path := "images/" + image.ID + ".png"
	old, readErr := root.ReadFile(path)
	if !bytes.Equal(old, image.Data) || readErr != nil {
		if err := root.WriteFile(path, image.Data, 0600); err != nil {
			return previewResult{}, err
		}
	}
	return previewResult{ImageID: image.ID, Workstream: op.Workstream, Commit: op.Commit, Path: filepath.ToSlash(path)}, nil
}

// restorePreviewImages reconciles receipts before a resumed chief reads its new
// view. A capture with no receipt runs again using its original source pin.
func restorePreviewImages(ctx context.Context, cfg *config.Config, controls *runtimeControls, repo *trace.Repository, scope coreadapter.Scope, view *isolation.FileView, now func() time.Time) error {
	docs, err := trace.Read[trace.Document](repo, config.WorkstreamID(scope.Workstream))
	if err != nil {
		return err
	}
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if doc.Revision > latest[doc.ID].Revision {
			latest[doc.ID] = doc
		}
	}
	var recovered []previewResult
	for _, doc := range latest {
		if !strings.HasPrefix(doc.ID, "preview-") {
			continue
		}
		var op previewOperation
		if err := json.Unmarshal([]byte(doc.Content), &op); err != nil {
			return err
		}
		// An interrupted chief is continued in a new physical turn. Reconcile its
		// durable preview before that continuation runs, using the original key.
		audit := latest[trace.ToolCallID(op.Scope, "preview", op.Call)]
		if op.Scope != scope && op.Result != nil && audit.Revision == 2 {
			continue
		}
		tool := controls.preview(cfg, repo, op.Scope, view, now)
		output, err := tool.Handle(ctx, op.Call)
		if err != nil {
			return err
		}
		var result previewResult
		if err := json.Unmarshal(output, &result); err != nil {
			return err
		}
		recovered = append(recovered, result)
	}
	if len(recovered) > 0 {
		data, _ := json.Marshal(recovered)
		root, err := os.OpenRoot(view.Workspace().Directory)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := root.MkdirAll("previews", 0700); err != nil {
			return err
		}
		return root.WriteFile("previews/results.json", data, 0600)
	}
	return nil
}

// finishPreviewTool completes a started audit even after the original turn was
// abandoned on restart. This is service reconciliation of an already accepted
// operation; it grants no new agent action on an inactive turn.
func finishPreviewTool(ctx context.Context, repo *trace.Repository, scope coreadapter.Scope, raw, output json.RawMessage, now func() time.Time) error {
	doc, err := latestPreviewDocument(repo, config.WorkstreamID(scope.Workstream), trace.ToolCallID(scope, "preview", raw))
	if err != nil {
		return err
	}
	if doc.Revision == 2 {
		return nil
	}
	if doc.ID == "" {
		return errors.New("preview has no started tool record")
	}
	var call trace.ToolCall
	if err := json.Unmarshal([]byte(doc.Content), &call); err != nil {
		return err
	}
	call.State, call.Failed = "completed", false
	call.Output = &trace.ToolPayload{Content: string(output), Bytes: len(output), SHA256: fmt.Sprintf("%x", sha256.Sum256(output))}
	data, _ := json.Marshal(call)
	doc.Revision, doc.At, doc.Content = 2, now(), string(data)
	return repo.RecordDocuments(ctx, []trace.Document{doc})
}

func previewDiagnostic(message string) string {
	message = redact.Secrets(message, redact.KnownSecrets()...)
	const limit = 16 << 10
	if len(message) > limit {
		message = message[len(message)-limit:]
		for len(message) > 0 && !utf8.RuneStart(message[0]) {
			message = message[1:]
		}
	}
	return message
}
