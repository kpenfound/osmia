package checks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

const (
	traceOne = "https://dagger.cloud/example/traces/first111"
	traceTwo = "https://dagger.cloud/example/traces/second22"
)

// recordedTraces collects what a TraceObserver hears.
type recordedTraces struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordedTraces) observe(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, url)
}

func (r *recordedTraces) heard() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.urls)
}

func TestTraceScannerReadsLinesAsTheyAreWritten(t *testing.T) {
	t.Parallel()
	write := func(s *traceScanner, chunks ...string) {
		for _, chunk := range chunks {
			if n, err := s.Write([]byte(chunk)); err != nil || n != len(chunk) {
				t.Fatalf("write %q: %d, %v", chunk, n, err)
			}
		}
	}
	for _, c := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"one line", []string{"Full trace at " + traceOne + "\n"}, traceOne},
		{"split in the host", []string{"Full trace at https://dagg", "er.cloud/example/traces/first111\n"}, traceOne},
		{"split in the path", []string{"Full trace at https://dagger.cloud/example/tr", "aces/first", "111\n"}, traceOne},
		{"one byte at a time", chunksOf("Full trace at " + traceOne + "\n"), traceOne},
		{"carriage returns", []string{"progress\rFull trace at " + traceOne + "\r\n"}, traceOne},
		{"latest of several", []string{traceOne + "\n" + traceTwo + "\n"}, traceTwo},
		{"a rejected link after an accepted one", []string{traceOne + "\n", "https://dagger.cloud/example/traces/x?token=1\n"}, traceOne},
		{"nothing", []string{"CHECKS  ✔ 1 passed\n"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTraceScanner("", nil)
			write(s, c.chunks...)
			if got := s.URL(); got != c.want {
				t.Fatalf("link %q, want %q", got, c.want)
			}
		})
	}
	t.Run("a last line without a newline is read once the output ends", func(t *testing.T) {
		s := newTraceScanner("", nil)
		write(s, "Full trace at ", traceOne)
		if s.URL() != "" {
			t.Fatalf("an unfinished line was read as a link: %q", s.URL())
		}
		s.Flush()
		if s.URL() != traceOne {
			t.Fatalf("link %q", s.URL())
		}
	})
}

func chunksOf(s string) []string {
	var chunks []string
	for _, r := range s {
		chunks = append(chunks, string(r))
	}
	return chunks
}

func TestTraceScannerKeepsAnEarlierLinkAndHearsEachChangeOnce(t *testing.T) {
	t.Parallel()
	var heard recordedTraces
	s := newTraceScanner(traceOne, heard.observe)
	s.Write([]byte("no link here\n" + traceOne + "\n"))
	if len(heard.heard()) != 0 || s.URL() != traceOne {
		t.Fatalf("a link already found is not new: %q, %q", heard.heard(), s.URL())
	}
	s.Write([]byte(traceTwo + "\n" + traceTwo + "\nplain\n"))
	if got := heard.heard(); !slices.Equal(got, []string{traceTwo}) || s.URL() != traceTwo {
		t.Fatalf("heard %q, link %q", got, s.URL())
	}
}

func TestTraceScannerBoundsAnUnfinishedLine(t *testing.T) {
	t.Parallel()
	s := newTraceScanner("", nil)
	filler := make([]byte, traceLineLimit*3)
	for i := range filler {
		filler[i] = 'x'
	}
	s.Write(filler)
	if len(s.partial) > traceLineLimit {
		t.Fatalf("held %d bytes", len(s.partial))
	}
	s.Write([]byte(" " + traceOne + "\n"))
	if s.URL() != traceOne {
		t.Fatalf("link after a long line: %q", s.URL())
	}
}

// tracedChecks is DaggerChecks with a token, which is what earns a trace link.
func tracedChecks(token string) DaggerChecks { return DaggerChecks{CloudToken: token} }

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksKeepTheLinkWhenTheRunDoesNotFinish(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		for _, end := range []string{"timeout", "cancellation"} {
			t.Run(stream+"/"+end, func(t *testing.T) {
				redirect := ""
				if stream == "stderr" {
					redirect = " >&2"
				}
				// exec leaves the sleep as the process the context kills.
				installLoggingDagger(t, "printf 'Full trace at "+traceOne+"\\n'"+redirect+"\nexec sleep 60\n")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := context.Canceled
				if end == "timeout" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 4*time.Second)
					defer stop()
					want = context.DeadlineExceeded
				}
				var heard recordedTraces
				ctx = WithTraceObserver(ctx, func(url string) {
					heard.observe(url)
					if end == "cancellation" {
						cancel()
					}
				})
				result, err := tracedChecks("trace-token").Check(ctx, t.TempDir(), Identity{}, nil)
				if !errors.Is(err, want) {
					t.Fatalf("err %v, want %v", err, want)
				}
				if result.TraceURL != traceOne {
					t.Fatalf("trace %q, output %q", result.TraceURL, result.Output)
				}
				if got := heard.heard(); !slices.Equal(got, []string{traceOne}) {
					t.Fatalf("heard %q", got)
				}
			})
		}
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksReadTheLinkFromEitherStreamAndAcrossWrites(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"stdout", "printf 'Full trace at " + traceOne + "\\n'\n"},
		{"stderr", "printf 'Full trace at " + traceOne + "\\n' >&2\n"},
		{"split writes", "printf 'Full trace at https://dagger.cl'\nsleep 0.2\nprintf 'oud/example/tra'\nsleep 0.2\nprintf 'ces/first111\\n'\n"},
		{"split stderr writes", "printf 'Full trace at https://dagger.cl' >&2\nsleep 0.2\nprintf 'oud/example/traces/first111\\n' >&2\n"},
		{"no final newline", "printf 'CHECKS  ✔ 1 passed\\nFull trace at " + traceOne + "'\n"},
		{"between other output", "printf 'out\\n'\nprintf 'Full trace at " + traceOne + " (note)\\n' >&2\nprintf 'CHECKS  ✔ 1 passed\\n'\n"},
		{"the link of a failed run", "printf 'Full trace at " + traceOne + "\\nCHECKS  ✘ 1 failed\\n✘ dag://failed 1s ERROR\\n    wrong\\n'\nexit 3\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			installLoggingDagger(t, c.body)
			var heard recordedTraces
			ctx := WithTraceObserver(context.Background(), heard.observe)
			result, err := tracedChecks("trace-token").Check(ctx, t.TempDir(), Identity{}, nil)
			must(t, err)
			if result.TraceURL != traceOne {
				t.Fatalf("trace %q, output %q", result.TraceURL, result.Output)
			}
			if got := heard.heard(); !slices.Equal(got, []string{traceOne}) {
				t.Fatalf("heard %q", got)
			}
		})
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksIgnoreURLsThatAreNotTraceLinks(t *testing.T) {
	for _, c := range []struct{ name, url string }{
		{"other host", "https://example.com/example/traces/abc"},
		{"host suffix", "https://dagger.cloud.evil/example/traces/abc"},
		{"plain http", "http://dagger.cloud/example/traces/abc"},
		{"user info", "https://secret@dagger.cloud/example/traces/abc"},
		{"query", "https://dagger.cloud/example/traces/abc?token=secret"},
		{"fragment", "https://dagger.cloud/example/traces/abc#secret"},
		{"port", "https://dagger.cloud:443/example/traces/abc"},
		{"not a trace path", "https://dagger.cloud/docs"},
		{"checks path", "https://dagger.cloud/checks/3.json"},
		{"deep path", "https://dagger.cloud/a/b/traces/abc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			installLoggingDagger(t, "printf 'Full trace at "+c.url+"\\n' >&2\nprintf '"+c.url+"\\n'\n")
			var heard recordedTraces
			ctx := WithTraceObserver(context.Background(), heard.observe)
			result, err := tracedChecks("trace-token").Check(ctx, t.TempDir(), Identity{}, nil)
			must(t, err)
			if result.TraceURL != "" || len(heard.heard()) != 0 {
				t.Fatalf("trace %q heard %q", result.TraceURL, heard.heard())
			}
		})
	}
	t.Run("an accepted link stays beside rejected ones", func(t *testing.T) {
		installLoggingDagger(t, "printf 'https://example.com/x/traces/y\\n"+traceOne+"\\nhttps://dagger.cloud/docs\\nhttps://dagger.cloud/example/traces/z?q=1\\n'\n")
		result, err := tracedChecks("trace-token").Check(context.Background(), t.TempDir(), Identity{}, nil)
		must(t, err)
		if result.TraceURL != traceOne {
			t.Fatalf("trace %q", result.TraceURL)
		}
	})
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksRecordNoLinkWithoutACloudToken(t *testing.T) {
	installLoggingDagger(t, "printf 'Full trace at "+traceOne+"\\nSetup tracing: https://dagger.cloud/traces/setup\\n' >&2\nprintf 'CHECKS  ✔ 1 passed\\n'\n")
	for _, engine := range []string{"", "auto", "tcp://127.0.0.1:1234"} {
		t.Run("engine "+engine, func(t *testing.T) {
			var heard recordedTraces
			ctx := WithTraceObserver(context.Background(), heard.observe)
			result, err := (DaggerChecks{Engine: engine}).Check(ctx, t.TempDir(), Identity{}, nil)
			must(t, err)
			if result.TraceURL != "" || len(heard.heard()) != 0 {
				t.Fatalf("trace %q heard %q", result.TraceURL, heard.heard())
			}
			if result.ExitCode != 0 || result.Output == "" {
				t.Fatalf("the run is unchanged: %+v", result)
			}
		})
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksCaptureTheLinkInEveryEngineMode(t *testing.T) {
	body := `case "$DAGGER_ENGINE" in
 cloud) kind=cloud ;;
 '') kind=auto ;;
 *) kind=runner ;;
esac
printf 'Full trace at https://dagger.cloud/example/traces/%s\n' "$kind" >&2
printf 'CHECKS  ✔ 1 passed\n'
`
	for _, c := range []struct{ engine, kind string }{
		{"", "auto"},
		{"auto", "auto"},
		{"tcp://127.0.0.1:1234", "runner"},
		{"cloud", "cloud"},
	} {
		t.Run("engine "+c.engine, func(t *testing.T) {
			installLoggingDagger(t, body)
			result, err := (DaggerChecks{Engine: c.engine, CloudToken: "trace-token"}).Check(t.Context(), t.TempDir(), Identity{}, nil)
			must(t, err)
			if want := "https://dagger.cloud/example/traces/" + c.kind; result.TraceURL != want {
				t.Fatalf("trace %q, want %q", result.TraceURL, want)
			}
		})
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksAnnounceTheLinkBeforeTheRunEnds(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "link-heard")
	// The fake waits to be told the link was heard, and fails if it never is.
	installLoggingDagger(t, fmt.Sprintf(`printf 'Full trace at %s\n'
n=0
until [ -e '%s' ]; do
 n=$((n+1)); [ "$n" -lt 200 ] || { printf 'link never heard\n'; exit 9; }
 sleep 0.05
done
printf 'CHECKS  ✔ 1 passed\n'
`, traceOne, gate))
	var heard recordedTraces
	ctx := WithTraceObserver(context.Background(), func(url string) {
		heard.observe(url)
		must(t, os.WriteFile(gate, nil, 0600))
	})
	result, err := tracedChecks("trace-token").Check(ctx, t.TempDir(), Identity{}, nil)
	must(t, err)
	if result.ExitCode != 0 || result.TraceURL != traceOne || !slices.Equal(heard.heard(), []string{traceOne}) {
		t.Fatalf("exit %d, trace %q, heard %q:\n%s", result.ExitCode, result.TraceURL, heard.heard(), result.Output)
	}
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksKeepTheLatestLinkAcrossAttempts(t *testing.T) {
	const (
		flags = "check --progress=report --fail-fast --parallel 4 "
		view  = "dag+check://?check=test&go-package=p"
		a     = "dag://?check=test&go-package=p&go-test=A"
		skipA = flags + "--skip " + a + " " + view
	)
	helpers := `fails() { printf 'CHECKS  ✘ 1 failed\n✘ %s 1s ERROR\n    went wrong\nRUN LOCALLY\ndagger check "%s"\n' "$1" "$1"; exit 1; }
passes() { printf 'CHECKS  ✔ 1 passed\n✔ %s 1s OK\n' "$1"; exit 0; }
`
	link := func(url string) string { return "printf 'Full trace at " + url + "\\n'; " }
	script := func(rerun, skip string) string {
		return helpers + "case \"$*\" in\n'" + flags + view + "') " + link(traceOne) + "fails '" + a + "' ;;\n" +
			"'" + flags + a + "') " + rerun + "passes '" + a + "' ;;\n" +
			"'" + skipA + "') " + skip + "passes 'dag://other' ;;\n*) exit 99 ;;\nesac\n"
	}
	run := func(t *testing.T, body string) (CheckResult, *recordedTraces) {
		t.Helper()
		installLoggingDagger(t, body)
		var heard recordedTraces
		ctx := WithTraceObserver(context.Background(), heard.observe)
		result, err := tracedChecks("trace-token").Check(ctx, t.TempDir(), Identity{}, []string{view})
		must(t, err)
		if !slices.Equal(result.Flaky, []string{a}) || result.ExitCode != 0 {
			t.Fatalf("expected a flaky pass: %+v", result)
		}
		return result, &heard
	}
	t.Run("a rerun and a continuation that print no link keep the first", func(t *testing.T) {
		result, heard := run(t, script("", ""))
		if result.TraceURL != traceOne || !slices.Equal(heard.heard(), []string{traceOne}) {
			t.Fatalf("trace %q heard %q", result.TraceURL, heard.heard())
		}
	})
	t.Run("a rerun that prints a link replaces it", func(t *testing.T) {
		result, heard := run(t, script(link(traceTwo), ""))
		if result.TraceURL != traceTwo || !slices.Equal(heard.heard(), []string{traceOne, traceTwo}) {
			t.Fatalf("trace %q heard %q", result.TraceURL, heard.heard())
		}
	})
	t.Run("a continuation that prints a link replaces it", func(t *testing.T) {
		result, _ := run(t, script("", link(traceTwo)))
		if result.TraceURL != traceTwo {
			t.Fatalf("trace %q", result.TraceURL)
		}
	})
	t.Run("a rerun that prints the same link is not new", func(t *testing.T) {
		result, heard := run(t, script(link(traceOne), ""))
		if result.TraceURL != traceOne || !slices.Equal(heard.heard(), []string{traceOne}) {
			t.Fatalf("trace %q heard %q", result.TraceURL, heard.heard())
		}
	})
}

// serial: installs a fake dagger executable through PATH.
func TestDaggerChecksKeepTheLinkOfAnEarlierInvocation(t *testing.T) {
	const second = "dag+check://release/version"
	for _, c := range []struct{ name, later, want string }{
		{"no link", "", traceOne},
		{"a link", "printf 'Full trace at " + traceTwo + "\\n'; ", traceTwo},
		{"a rejected link", "printf 'Full trace at https://example.com/o/traces/z\\n'; ", traceOne},
	} {
		t.Run(c.name, func(t *testing.T) {
			installLoggingDagger(t, "case \"$*\" in\n*"+second+") "+c.later+"printf 'CHECKS  ✔ 1 passed\\n' ;;\n*) printf 'Full trace at "+traceOne+"\\nCHECKS  ✔ 1 passed\\n' ;;\nesac\n")
			result, err := tracedChecks("trace-token").Check(context.Background(), t.TempDir(), Identity{}, []string{"dag+check://?check=test&go-package=p", second})
			must(t, err)
			if result.TraceURL != c.want {
				t.Fatalf("trace %q, want %q", result.TraceURL, c.want)
			}
		})
	}
}

// printedBy is a runner that, as Dagger does, makes its link known while it runs.
func printedBy(url string, then func(ctx context.Context) (CheckResult, error)) func(context.Context, fakeCall) (CheckResult, error) {
	return func(ctx context.Context, _ fakeCall) (CheckResult, error) {
		if observe := traceObserverFrom(ctx); observe != nil && url != "" {
			observe(url)
		}
		result, err := then(ctx)
		if url != "" {
			result.TraceURL = url
		}
		return result, err
	}
}

func TestFallbackResultLinksToTheCloudRunOrKeepsTheLocalLink(t *testing.T) {
	t.Parallel()
	timedOut := func(ctx context.Context) (CheckResult, error) {
		<-ctx.Done()
		return unfinished(), ctx.Err()
	}
	passed := func(context.Context) (CheckResult, error) { return passes(linkA), nil }
	for _, c := range []struct {
		name, local, cloud, want string
		heard                    []string
	}{
		{"cloud printed a link", traceOne, traceTwo, traceTwo, []string{traceOne, traceTwo}},
		{"cloud printed no link", traceOne, "", traceOne, []string{traceOne}},
		{"only cloud printed a link", "", traceTwo, traceTwo, []string{traceTwo}},
		{"neither printed a link", "", "", "", nil},
		{"the same link twice", traceOne, traceOne, traceOne, []string{traceOne}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			local := fake(printedBy(c.local, timedOut))
			cloud := fake(printedBy(c.cloud, passed))
			var heard recordedTraces
			ctx := WithTraceObserver(t.Context(), heard.observe)
			result, _, attribution, err := RunSelectionWithFallback(ctx, local, cloud, testBudget, Remembered{}, t.TempDir(), Identity{}, api.CheckSelection{Mode: SelectionSelected, Links: []string{linkA}, Judgment: "j"})
			must(t, err)
			if len(attribution.CloudRuns()) != 1 {
				t.Fatalf("attribution %+v", attribution)
			}
			if result.TraceURL != c.want {
				t.Fatalf("trace %q, want %q", result.TraceURL, c.want)
			}
			if !slices.Equal(heard.heard(), c.heard) {
				t.Fatalf("heard %q, want %q", heard.heard(), c.heard)
			}
		})
	}
}

func TestFallbackHearsALinkARunnerReturnsWithoutStreaming(t *testing.T) {
	t.Parallel()
	for name, withCloud := range map[string]bool{"without cloud": false, "with cloud": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result := passes(linkA)
			result.TraceURL = traceOne
			local := fake(always(result, nil))
			var cloud ReviewChecks
			if withCloud {
				cloud = fake(always(passes(linkA), nil))
			}
			var heard recordedTraces
			ctx := WithTraceObserver(t.Context(), heard.observe)
			got, _, _, err := RunSelectionWithFallback(ctx, local, cloud, testBudget, Remembered{}, t.TempDir(), Identity{}, api.CheckSelection{Mode: SelectionSelected, Links: []string{linkA}, Judgment: "j"})
			must(t, err)
			if got.TraceURL != traceOne || !slices.Equal(heard.heard(), []string{traceOne}) {
				t.Fatalf("trace %q heard %q", got.TraceURL, heard.heard())
			}
		})
	}
}

// serial: installs a fake dagger executable through PATH.
func TestFallbackWithDaggerChecksFollowsTheCloudRunsLink(t *testing.T) {
	for _, c := range []struct {
		name, cloudLink, want string
		heard                 []string
	}{
		{"cloud printed a link", "printf 'Full trace at " + traceTwo + "\\n' >&2; ", traceTwo, []string{traceOne, traceTwo}},
		{"cloud printed no link", "", traceOne, []string{traceOne}},
	} {
		t.Run(c.name, func(t *testing.T) {
			installLoggingDagger(t, `if [ "$DAGGER_ENGINE" = cloud ]; then
 `+c.cloudLink+`printf 'CHECKS  ✔ 1 passed\n'; exit 0
fi
printf 'Full trace at `+traceOne+`\n' >&2
printf 'engine closed session\n'
exit 7
`)
			local := DaggerChecks{Engine: "tcp://127.0.0.1:1234", CloudToken: "trace-token"}
			cloud := DaggerChecks{Engine: "cloud", CloudToken: "trace-token"}
			var heard recordedTraces
			ctx := WithTraceObserver(t.Context(), heard.observe)
			result, _, attribution, err := RunSelectionWithFallback(ctx, local, cloud, time.Minute, Remembered{}, t.TempDir(), Identity{}, FullSelection("unlisted", ""))
			must(t, err)
			if len(attribution.CloudRuns()) != 1 || result.ExitCode != 0 {
				t.Fatalf("exit %d, attribution %+v", result.ExitCode, attribution)
			}
			if result.TraceURL != c.want || !slices.Equal(heard.heard(), c.heard) {
				t.Fatalf("trace %q, heard %q", result.TraceURL, heard.heard())
			}
		})
	}
}
