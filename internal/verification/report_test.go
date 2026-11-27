package verification

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadReturnsTheClaimsStatusesAndEvidenceOfAWellFormedReport(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"report.json": `{"version":1,"claims":[
 {"name":"a","status":"pass","detail":"ok","evidence":["shots/a.png","logs/a.txt"],"flow":"checkout"},
 {"name":"b","status":"fail","detail":"bad","evidence":["logs/a.txt","./logs/b.txt"]},
 {"name":"c","status":"inconclusive","detail":"?","evidence":[]}]}`,
		"shots/a.png": "png", "logs/a.txt": "a", "logs/b.txt": "b",
	})
	r := Load(dir)
	if r.Rejected != "" || len(r.Claims) != 3 {
		t.Fatalf("result %+v", r)
	}
	if r.Claims[0].Flow != "checkout" || r.Claims[1].Status != Fail || r.Claims[1].Flow != "" || r.Claims[2].Status != Inconclusive {
		t.Fatalf("claims %+v", r.Claims)
	}
	if want := []string{"shots/a.png", "logs/a.txt", "logs/b.txt"}; !slices.Equal(r.Evidence, want) {
		t.Fatalf("evidence %q", r.Evidence)
	}
	if r.Status() != Fail {
		t.Fatalf("status %q", r.Status())
	}
}

func TestLoadRejectsAReportWithTheReason(t *testing.T) {
	claim := func(extra string) string {
		return `{"version":1,"claims":[{"name":"a","status":"pass","detail":"d","evidence":[` + extra + `]}]}`
	}
	for name, tc := range map[string]struct {
		files  map[string]string
		reason string
	}{
		"no report":             {map[string]string{"x.txt": "x"}, "is missing"},
		"malformed":             {map[string]string{"report.json": `{"version":`}, "not valid JSON"},
		"not an object":         {map[string]string{"report.json": `[]`}, "not valid JSON"},
		"two documents":         {map[string]string{"report.json": `{"version":1,"claims":[]}{}`}, "not valid JSON"},
		"no version":            {map[string]string{"report.json": `{"claims":[]}`}, "no version"},
		"future version":        {map[string]string{"report.json": `{"version":2,"claims":"anything"}`}, "unsupported version 2"},
		"no claims list":        {map[string]string{"report.json": `{"version":1}`}, "no claims list"},
		"claim without name":    {map[string]string{"report.json": `{"version":1,"claims":[{"status":"pass"}]}`}, "has no name"},
		"claim without status":  {map[string]string{"report.json": `{"version":1,"claims":[{"name":"a"}]}`}, "has no status"},
		"unknown status":        {map[string]string{"report.json": `{"version":1,"claims":[{"name":"a","status":"PASS"}]}`}, `unknown status "PASS"`},
		"wrong detail type":     {map[string]string{"report.json": `{"version":1,"claims":[{"name":"a","status":"pass","detail":3}]}`}, "wrong type"},
		"empty flow":            {map[string]string{"report.json": `{"version":1,"claims":[{"name":"a","status":"pass","flow":" "}]}`}, "empty flow"},
		"absent evidence":       {map[string]string{"report.json": claim(`"none.txt"`)}, "is not in the Directory"},
		"parent evidence":       {map[string]string{"report.json": claim(`"../x"`)}, "escapes"},
		"nested parent":         {map[string]string{"report.json": claim(`"a/../../x"`)}, "escapes"},
		"absolute evidence":     {map[string]string{"report.json": claim(`"/etc/passwd"`)}, "relative"},
		"home evidence":         {map[string]string{"report.json": claim(`"~/x"`)}, "relative"},
		"backslash evidence":    {map[string]string{"report.json": claim(`"a\\b"`)}, "separators"},
		"empty evidence":        {map[string]string{"report.json": claim(`""`)}, "empty"},
		"evidence is the dir":   {map[string]string{"report.json": claim(`"."`)}, "escapes"},
		"evidence is report":    {map[string]string{"report.json": claim(`"report.json"`)}, "the report itself"},
		"evidence is directory": {map[string]string{"report.json": claim(`"logs"`), "logs/a": "a"}, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			r := Load(writeDir(t, tc.files))
			if !strings.Contains(r.Rejected, tc.reason) {
				t.Fatalf("rejected %q, want %q", r.Rejected, tc.reason)
			}
			if r.Status() != Inconclusive || len(r.Claims) != 0 || len(r.Evidence) != 0 {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

func TestLoadRefusesEvidenceAndReportsThatLeaveTheDirectoryBySymlink(t *testing.T) {
	outside := writeDir(t, map[string]string{"secret.txt": "host", "report.json": `{"version":1,"claims":[]}`})
	dir := writeDir(t, map[string]string{"report.json": `{"version":1,"claims":[{"name":"a","status":"pass","evidence":["link/secret.txt"]}]}`})
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if r := Load(dir); r.Rejected == "" {
		t.Fatalf("evidence through a symlink was accepted: %+v", r)
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "report.json"), filepath.Join(linked, "report.json")); err != nil {
		t.Fatal(err)
	}
	if r := Load(linked); r.Rejected == "" {
		t.Fatalf("a report symlinked outside was accepted: %+v", r)
	}
}

func TestLoadRejectsAnOversizedReport(t *testing.T) {
	dir := writeDir(t, map[string]string{"report.json": `{"version":1,"claims":[{"name":"` + strings.Repeat("a", MaxReportBytes) + `","status":"pass"}]}`})
	if r := Load(dir); !strings.Contains(r.Rejected, "larger than") {
		t.Fatalf("result %+v", r)
	}
}

func TestLoadOfAMissingDirectoryIsRejected(t *testing.T) {
	if r := Load(filepath.Join(t.TempDir(), "none")); r.Rejected == "" || r.Status() != Inconclusive {
		t.Fatalf("result %+v", r)
	}
}

func TestParseReportAllowsClaimsWithoutDetailOrEvidence(t *testing.T) {
	r, err := ParseReport([]byte(`{"version":1,"claims":[{"name":"a","status":"pass"}]}`), func(string) error { return errors.New("no files") })
	if err != nil || len(r.Claims) != 1 || r.Claims[0].Evidence == nil || len(r.Evidence) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
