package web

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPageKeepsEveryTestHook(t *testing.T) {
	list, err := os.ReadFile("testdata/hooks.txt")
	if err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	for _, path := range []string{"/", "/app.js"} {
		w, _ := serve(http.MethodGet, path)
		source.WriteString(w.Body.String())
	}
	var missing []string
	hooks := 0
	for _, line := range strings.Split(strings.TrimSpace(string(list)), "\n") {
		attribute, value, _ := strings.Cut(line, "\t")
		hooks++
		found := regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(attribute) + `["']?\s*[=:,]\s*\\?["']` + regexp.QuoteMeta(value) + `["']`)
		if !found.MatchString(source.String()) {
			missing = append(missing, attribute+"="+value)
		}
	}
	if hooks < 250 {
		t.Fatalf("testdata/hooks.txt lists %d hooks, want the page's whole set", hooks)
	}
	if len(missing) > 0 {
		t.Fatalf("the page lost the test hooks %q", missing)
	}
}

func TestPageLoadsOnlyFromItsOwnOrigin(t *testing.T) {
	w, _ := serve(http.MethodGet, "/")
	page := w.Body.String()
	for _, tag := range regexp.MustCompile(`<(?:script|link)\b[^>]*>`).FindAllString(page, -1) {
		for _, attr := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(tag, -1) {
			if !strings.HasPrefix(attr[1], "/") || strings.HasPrefix(attr[1], "//") {
				t.Fatalf("the page loads %s from outside its own origin", attr[1])
			}
		}
	}
}

func TestScriptIsNotAModuleAndLoadsNoPackages(t *testing.T) {
	script, _ := serve(http.MethodGet, "/app.js")
	if regexp.MustCompile(`(?m)^\s*(import\s|export\s)|\brequire\(`).MatchString(script.Body.String()) {
		t.Fatal("the script is a module or loads packages")
	}
}

func TestPageSourcesJoinNoMetadataWithASeparator(t *testing.T) {
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w, _ := serve(http.MethodGet, path)
		for n, line := range strings.Split(w.Body.String(), "\n") {
			if strings.Contains(line, " · ") || strings.Contains(line, "\\u00b7") {
				t.Fatalf("%s line %d joins metadata with a separator: %s", path, n+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestPageTextHoldsNoCapitalisedLabels(t *testing.T) {
	page, _ := serve(http.MethodGet, "/")
	text := regexp.MustCompile(`>([^<>]+)<`)
	allowed := map[string]bool{"ID": true, "URL": true, "AGENTS": true, "GitHub": true, "HTTPS": true}
	for _, m := range text.FindAllStringSubmatch(page.Body.String(), -1) {
		for _, word := range strings.Fields(m[1]) {
			word = strings.Trim(word, `.,:;!?()"'`)
			if len(word) >= 4 && word == strings.ToUpper(word) && regexp.MustCompile(`^[A-Z]+$`).MatchString(word) && !allowed[word] {
				t.Fatalf("the page text %q holds the capitals %q", strings.TrimSpace(m[1]), word)
			}
		}
	}
}

func TestPageSourceLabelsAreInSentenceCase(t *testing.T) {
	names := map[string]bool{"Osmia": true, "Dagger": true, "GitHub": true, "Jev": true, "Git": true, "Docker": true, "Tailscale": true, "Chromium": true, "Cloud": true, "I": true, "OK": true}
	later := regexp.MustCompile(`^[A-Z][a-z]`)
	check := func(source, text string) {
		t.Helper()
		words := strings.Fields(text)
		for _, word := range words[min(1, len(words)):] {
			word = strings.Trim(word, `.,:;!?()"'…`)
			if later.MatchString(word) && !names[word] {
				t.Errorf("%s: %q capitalises %q after its first word", source, strings.Join(words, " "), word)
			}
		}
	}
	page, _ := serve(http.MethodGet, "/")
	for _, m := range regexp.MustCompile(`<(?:button|h[1-4]|label|legend|summary|th|option)\b[^>]*>([^<]+)<`).FindAllStringSubmatch(page.Body.String(), -1) {
		check("index.html", m[1])
	}
	script, _ := serve(http.MethodGet, "/app.js")
	for _, m := range regexp.MustCompile(`el\('(?:button|h[1-4]|label|legend|summary|th|option)'[^\n]*?'([A-Z][^'\n]{3,60})'\)`).FindAllStringSubmatch(script.Body.String(), -1) {
		check("app.js", m[1])
	}
}
