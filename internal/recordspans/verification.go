package recordspans

import (
	"encoding/json"
	"strconv"

	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
)

// The outcomes of a flow span of a final verification measure: a failed flow
// and an inconclusive one are separate measures, and an inconclusive flow is
// never counted as a failure.
const (
	FlowFail         = "fail"
	FlowInconclusive = "inconclusive"
)

func (r Recorder) verification(d trace.CommitDetail, doc trace.Document) {
	if unit, ok := verification.ParseUnitMeasurePath(doc.Path); ok {
		r.unitMeasure(d, doc, unit)
	} else if verification.IsFinalMeasurePath(doc.Path) {
		r.finalMeasure(d, doc)
	}
}

func (r Recorder) unitMeasure(d trace.CommitDetail, doc trace.Document, unit string) {
	var m verification.UnitMeasure
	if json.Unmarshal([]byte(doc.Content), &m) != nil {
		return
	}
	w := d.Workstream
	count := telemetry.VerifyCount
	attrs := []telemetry.Attr{
		telemetry.Project(d.Project), telemetry.Workstream(w), telemetry.Unit(unit), telemetry.Record(doc.ID),
		count(telemetry.KeyVerifyRuns, m.Runs.Total()), count(telemetry.KeyVerifyPass, m.Runs.Pass), count(telemetry.KeyVerifyFail, m.Runs.Fail),
		count(telemetry.KeyVerifyInconclusive, m.Runs.Inconclusive), count(telemetry.KeyVerifyLimited, m.Limited),
		count(telemetry.KeyVerifyDeclaredRuns, m.Declared.Runs.Total()), count(telemetry.KeyVerifyDeclaredFlows, len(m.Declared.Flows)), count(telemetry.KeyVerifyDeclaredUntagged, m.Declared.UntaggedRuns),
		count(telemetry.KeyVerifyReviewerRuns, m.Reviewer.Runs.Total()), count(telemetry.KeyVerifyReviewerFlows, len(m.Reviewer.Flows)), count(telemetry.KeyVerifyReviewerUntagged, m.Reviewer.UntaggedRuns),
		count(telemetry.KeyVerifySkipped, m.Declared.Skipped+m.Reviewer.Skipped), count(telemetry.KeyVerifyRefused, m.Declared.Refused+m.Reviewer.Refused),
		telemetry.VerifyFlag(telemetry.KeyVerifyLeverTouched, m.LeverTouched), telemetry.VerifyFlag(telemetry.KeyVerifyMapTouched, m.MapTouched),
	}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.SpanName("verify", "unit"), Trace: telemetry.WorkstreamTrace(w),
		ID:     telemetry.RecordSpan(w, doc.ID, doc.Revision),
		Parent: telemetry.WorkstreamRoot(w), Start: doc.At, End: doc.At, Attrs: attrs,
	})
}

func (r Recorder) finalMeasure(d trace.CommitDetail, doc trace.Document) {
	var m verification.FinalMeasure
	if json.Unmarshal([]byte(doc.Content), &m) != nil {
		return
	}
	w := d.Workstream
	count := telemetry.VerifyCount
	base := []telemetry.Attr{telemetry.Project(d.Project), telemetry.Workstream(w), telemetry.Record(doc.ID)}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.SpanName("verify", "final"), Trace: telemetry.WorkstreamTrace(w),
		ID:     telemetry.RecordSpan(w, doc.ID, doc.Revision),
		Parent: telemetry.WorkstreamRoot(w), Start: doc.At, End: doc.At,
		Attrs: append(append([]telemetry.Attr(nil), base...),
			count(telemetry.KeyVerifyRuns, m.Runs.Total()), count(telemetry.KeyVerifyPass, m.Runs.Pass), count(telemetry.KeyVerifyFail, m.Runs.Fail),
			count(telemetry.KeyVerifyInconclusive, m.Runs.Inconclusive), count(telemetry.KeyVerifyLimited, m.Limited),
			count(telemetry.KeyVerifyFailedFlows, len(m.Failed)), count(telemetry.KeyVerifyInconclusiveFlows, len(m.Inconclusive)),
			count(telemetry.KeyVerifyUnattributedFails, m.Unattributed()),
			count(telemetry.KeyVerifyFailingUnits, len(m.UnitsFailing)), count(telemetry.KeyVerifyInconclusiveUnits, len(m.UnitsInconclusive))),
	})
	for _, group := range []struct {
		outcome string
		flows   []verification.FlowAttribution
	}{{FlowFail, m.Failed}, {FlowInconclusive, m.Inconclusive}} {
		for _, f := range group.flows {
			r.Sink.Emit(telemetry.Span{
				Name: telemetry.SpanName("verify", "final_flow"), Trace: telemetry.WorkstreamTrace(w),
				ID:     telemetry.WorkstreamSpan(w, "verify-final-flow", doc.ID, strconv.Itoa(doc.Revision), group.outcome, f.Flow),
				Parent: telemetry.WorkstreamRoot(w), Start: doc.At, End: doc.At,
				Attrs: append(append([]telemetry.Attr(nil), base...), telemetry.VerifyFlow(f.Flow), telemetry.Outcome(group.outcome),
					telemetry.VerifyUnits(f.Units), telemetry.VerifyFlag(telemetry.KeyVerifyShared, f.Shared), telemetry.VerifyFlag(telemetry.KeyVerifyUnattributed, f.Unattributed)),
				Failed: group.outcome == FlowFail,
			})
		}
	}
	for _, group := range []struct {
		outcome string
		units   []string
		flows   []verification.FlowAttribution
	}{{FlowFail, m.UnitsFailing, m.Failed}, {FlowInconclusive, m.UnitsInconclusive, m.Inconclusive}} {
		for _, unit := range group.units {
			shared := false
			for _, f := range group.flows {
				for _, u := range f.Units {
					shared = shared || u == unit && f.Shared
				}
			}
			r.Sink.Emit(telemetry.Span{
				Name: telemetry.SpanName("verify", "final_unit"), Trace: telemetry.WorkstreamTrace(w),
				ID:     telemetry.WorkstreamSpan(w, "verify-final-unit", doc.ID, strconv.Itoa(doc.Revision), group.outcome, unit),
				Parent: telemetry.WorkstreamRoot(w), Start: doc.At, End: doc.At,
				Attrs: append(append([]telemetry.Attr(nil), base...), telemetry.Unit(unit), telemetry.Outcome(group.outcome), telemetry.VerifyFlag(telemetry.KeyVerifyShared, shared)),
			})
		}
	}
}
