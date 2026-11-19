package checks

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
)

func TestTraceCommandIsGivenOnlyForAPlainTraceID(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{
		"4e3a3896a86e28f2204157939fb07b9f": "dagger trace 4e3a3896a86e28f2204157939fb07b9f",
		"trace_A-1":                        "dagger trace trace_A-1",
		"":                                 "",
		"abc; rm -rf /":                    "",
		"abc def":                          "",
	} {
		if got := TraceCommand(id); got != want {
			t.Errorf("TraceCommand(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSafeLinkCarriesNoCredential(t *testing.T) {
	t.Parallel()
	const check = "https://dagger.cloud/acme/checks/github.com/acme/widgets@abc?check=test:a"
	if got := SafeLink(check); got != check {
		t.Errorf("a plain status link changed: %q", got)
	}
	for _, link := range []string{check + "&token=s3cret", check + "&API_KEY=s3cret#frag", check + "&X-Amz-Signature=s3cret"} {
		got := SafeLink(link)
		if got == "" || strings.Contains(got, "s3cret") || strings.Contains(got, "#") || !strings.HasPrefix(got, "https://dagger.cloud/acme/checks/github.com/acme/widgets@abc?check=") {
			t.Errorf("SafeLink(%q) = %q", link, got)
		}
	}
	for _, link := range []string{"https://user:pw@dagger.cloud/x", "https://token@dagger.cloud/x?check=a", "ftp://dagger.cloud/x", "not a url", ""} {
		if got := SafeLink(link); got != "" {
			t.Errorf("SafeLink(%q) = %q, want none", link, got)
		}
	}
}

func TestSafeTextCarriesNoCredentialInAnyLink(t *testing.T) {
	t.Parallel()
	got := SafeText("read https://dagger.cloud/run.json?token=s3cret&x=1, then https://u:s3cret@dagger.cloud/y. and (https://dagger.cloud/z?api_key=s3cret) done")
	want := "read https://dagger.cloud/run.json?x=1, then [link withheld]. and (https://dagger.cloud/z) done"
	if got != want {
		t.Errorf("SafeText = %q, want %q", got, want)
	}
	if plain := "the module did not load"; SafeText(plain) != plain {
		t.Errorf("text without a link changed: %q", SafeText(plain))
	}
}

func TestDescribeFailedChecks(t *testing.T) {
	t.Parallel()
	failed := []api.CheckFailure{
		{Name: "test:a", TraceID: "ta", TargetURL: "https://dagger.cloud/x?check=test:a"},
		{Name: "lint", TargetURL: "https://u:p@dagger.cloud/x?check=lint"},
		{Name: "build", TargetURL: "https://dagger.cloud/x?check=build"},
	}
	got := DescribeFailedChecks(failed, nil, "", true)
	for _, want := range []string{"Failed checks (3):", "- test:a: `dagger trace ta`", "- lint: Dagger Cloud posted no trace id for this check\n", "- build: Dagger Cloud posted no trace id for this check; status: https://dagger.cloud/x?check=build"} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "u:p") {
		t.Errorf("a link carries userinfo:\n%s", got)
	}

	none := DescribeFailedChecks(nil, &api.CheckCounts{Total: 5, Passed: 4, Failed: 1}, "https://dagger.cloud/run.json", true)
	if !strings.Contains(none, "no failing check status was found") || !strings.Contains(none, "counts 1 failed of 5 checks") || !strings.Contains(none, "https://dagger.cloud/run.json") || strings.Contains(none, "Failed checks (") {
		t.Errorf("no failing status:\n%s", none)
	}
	if got := DescribeFailedChecks(nil, nil, "", false); got != "" {
		t.Errorf("a run that did not fail: %q", got)
	}
}
