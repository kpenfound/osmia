package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/trace"
)

// UnitCheckSelection records a run's choice before its check runner starts.
// It has no result; only the separate UnitCheckRun establishes completion.
type UnitCheckSelection struct {
	Unit       string         `json:"unit"`
	Run        int            `json:"run"`
	Operation  string         `json:"operation"`
	Candidate  string         `json:"candidate"`
	Base       string         `json:"base"`
	DiffSHA256 string         `json:"diff_sha256"`
	Report     int            `json:"report"`
	Selection  CheckSelection `json:"selection"`
	StartedAt  time.Time      `json:"started_at"`

	// TraceURL is the Dagger Cloud trace link the run printed, when the start
	// record carries one.
	TraceURL string `json:"trace_url,omitempty"`
}

func CheckSelectionDocument(unit string, run int) string {
	return CheckDocumentID(unit, run) + "-selection"
}

func CheckSelectionPath(unit string, run int) string {
	return fmt.Sprintf("units/%s/checks-%d-selection.json", unit, run)
}

func (c *checkers) latestSelection(in checkInput, run *UnitCheckRun) (UnitCheckSelection, trace.Document, bool, error) {
	docs, err := trace.Read[trace.Document](c.repository, in.Workstream)
	if err != nil {
		return UnitCheckSelection{}, trace.Document{}, false, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == CheckSelectionDocument(in.Unit, in.Run) && d.Revision > latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return UnitCheckSelection{}, trace.Document{}, false, nil
	}
	var prior UnitCheckSelection
	if err := json.Unmarshal([]byte(latest.Content), &prior); err != nil {
		return UnitCheckSelection{}, trace.Document{}, false, fmt.Errorf("%s: %w", latest.Path, err)
	}
	if prior.Unit != in.Unit || prior.Run != in.Run || prior.Operation != run.Operation || prior.Candidate != in.Candidate || prior.Base != in.Base || prior.Report != in.Report || (run.DiffSHA256 != "" && prior.DiffSHA256 != run.DiffSHA256) {
		return UnitCheckSelection{}, trace.Document{}, false, fmt.Errorf("%s does not match the check run", latest.Path)
	}
	return prior, latest, true, nil
}

func (c *checkers) restoreSelection(in checkInput, run *UnitCheckRun) (bool, error) {
	prior, _, found, err := c.latestSelection(in, run)
	if err != nil || !found {
		return false, err
	}
	run.Selection = &prior.Selection
	run.StartedAt = prior.StartedAt
	run.DiffSHA256 = prior.DiffSHA256
	if prior.TraceURL != "" {
		run.TraceURL = prior.TraceURL
	}
	return true, nil
}

func (c *checkers) recordTrace(ctx context.Context, in checkInput, run *UnitCheckRun, link string) error {
	prior, latest, found, err := c.latestSelection(in, run)
	if err != nil || !found || prior.TraceURL == link {
		return err
	}
	prior.TraceURL = link
	data, err := json.MarshalIndent(prior, "", "  ")
	if err != nil {
		return err
	}
	h := latest.Header
	h.Revision, h.At = latest.Revision+1, c.s.Now()
	return c.repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: latest.Path, Content: string(data) + "\n"}})
}

func (c *checkers) selection(ctx context.Context, in checkInput, run *UnitCheckRun, dir, diff string, checks ReviewChecks) (CheckSelection, error) {
	if found, err := c.restoreSelection(in, run); err != nil {
		return CheckSelection{}, err
	} else if found {
		return *run.Selection, nil
	}
	selection := c.selectChecks(ctx, in, dir, diff, checks)
	evidence := UnitCheckSelection{Unit: in.Unit, Run: in.Run, Operation: run.Operation, Candidate: in.Candidate, Base: in.Base, Report: in.Report, DiffSHA256: run.DiffSHA256, Selection: selection, StartedAt: run.StartedAt}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return CheckSelection{}, err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: CheckSelectionDocument(in.Unit, in.Run), Revision: 1, Project: c.repository.Project(), Workstream: in.Workstream, Unit: in.Unit, At: c.s.Now(), Actor: ChecksActor, Cause: checkRequestID(in.Unit, in.Run)}, Path: CheckSelectionPath(in.Unit, in.Run), Content: string(data) + "\n"}
	if err := c.repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if found, readErr := c.restoreSelection(in, run); readErr != nil {
				return CheckSelection{}, readErr
			} else if found {
				return *run.Selection, nil
			}
		}
		return CheckSelection{}, err
	}
	return selection, nil
}

type traceWatch struct {
	c      *checkers
	ctx    context.Context
	in     checkInput
	run    UnitCheckRun
	wake   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	latest string
}

func (c *checkers) watchTraces(ctx context.Context, in checkInput, run UnitCheckRun) *traceWatch {
	w := &traceWatch{c: c, ctx: ctx, in: in, run: run, wake: make(chan struct{}, 1), done: make(chan struct{}), latest: run.TraceURL}
	go w.write()
	return w
}

func (w *traceWatch) observed(ctx context.Context) context.Context {
	return checkrun.WithTraceObserver(ctx, w.observe)
}

func (w *traceWatch) observe(url string) {
	w.mu.Lock()
	w.latest = url
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *traceWatch) write() {
	defer close(w.done)
	ctx := context.WithoutCancel(w.ctx)
	written := w.run.TraceURL
	flush := func() {
		w.mu.Lock()
		link := w.latest
		w.mu.Unlock()
		if link != "" && link != written && w.c.recordTrace(ctx, w.in, &w.run, link) == nil {
			written = link
		}
	}
	for range w.wake {
		flush()
	}
	flush()
}

func (w *traceWatch) stop() {
	close(w.wake)
	<-w.done
}
