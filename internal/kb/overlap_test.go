package kb

import "testing"

func TestPatternsOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"internal/trace", "internal/trace/git.go", true},
		{"internal/trace", "internal/kb", false},
		{"src/*.go", "src/test*.go", true},
		{"src/*.go", "src/*.md", false},
		{"src/**/test?.go", "src/a/test[ab].go", true},
		{"src/**/test?.go", "docs/testa.go", false},
		{"src/[ab].go", "src/[bc].go", true},
		{"src/[ab].go", "src/[cd].go", false},
		{"src/[!a].go", "src/b.go", false}, // path.Match uses ^, not !, for negation.
		{"src/[^a].go", "src/b.go", true},
		{"**/*.go", "cmd/osmia/main.go", true},
		{"README.md", "docs/guide.md", false},
	} {
		if got := PatternsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("PatternsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := PatternsOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("PatternsOverlap(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}
