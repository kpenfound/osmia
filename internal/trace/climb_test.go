package trace

import (
	"context"
	"testing"
)

func TestClimbAttemptDocumentPathsAreValidated(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	first := streamDocument(ClimbAttemptID(1), ClimbAttemptPath(1), "{\"number\":1}\n", 1)
	if err := r.RecordDocuments(ctx, []Document{first}); err != nil {
		t.Fatal(err)
	}
	if got := fileOf(t, mustScoped(t, r, streamID), streamFile("climb/attempts/1.json")); got != "{\"number\":1}\n" {
		t.Fatalf("climb/attempts/1.json: %q", got)
	}
	for _, path := range []string{"climb/attempts/0.json", "climb/attempts/01.json", "climb/attempts/x.json", "climb/attempts/1.txt", "climb/attempts/1", "climb/attempts", "climb/other/1.json", "climb/attempts/1/2.json", "attempts/1.json"} {
		if err := r.RecordDocuments(ctx, []Document{streamDocument("climb-attempt-other", path, "x\n", 1)}); err == nil {
			t.Fatalf("%s accepted", path)
		}
	}
	project := projectDocument("climb-attempt-1", ClimbAttemptPath(1), "x\n", 1)
	if err := r.RecordDocuments(ctx, []Document{project}); err == nil {
		t.Fatal("a project document at a climb attempt path was accepted")
	}
}

func TestClimbCalibrationAndRunningDocumentsAreRecordedAtTheirPaths(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	for id, path := range map[string]string{"climb-calibration": ClimbCalibrationPath, "climb-running": ClimbRunningPath} {
		if err := r.RecordDocuments(ctx, []Document{streamDocument(id, path, "{}\n", 1)}); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	for _, path := range []string{"climb/other.json", "climb/calibration", "climb/running.json/x"} {
		if err := r.RecordDocuments(ctx, []Document{streamDocument("climb-other", path, "x\n", 1)}); err == nil {
			t.Fatalf("%s accepted", path)
		}
	}
}
