package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gib = uint64(1) << 30

// sizing runs doctor on a host with total bytes of memory and the roles
// configuration appended to the base one.
func sizing(t *testing.T, total uint64, roles string) []Check {
	t.Helper()
	o, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf(topLevel, projectID)+roles), 0o600))
	o.TotalMemory = func() (uint64, error) { return total, nil }
	return Run(context.Background(), o)
}

const sizedRoles = `
[roles.mason]
sandbox = "sbx"
memory = "4g"
[roles.reviewer]
sandbox = "sbx"
memory = "2g"
[roles.committee]
sandbox = "sbx"
memory = "2g"
[roles.architect]
sandbox = "sbx"
memory = "2g"
[roles.librarian]
sandbox = "sbx"
memory = "2g"
`

func TestSandboxMemoryAboveHostWarns(t *testing.T) {
	// 4 masons × 4g + 2 reviewers × 2g + 3 committee × 2g + architect and
	// librarian once each at 2g = 30 GiB.
	checks := sizing(t, 16*gib, sizedRoles)
	c := find(t, checks, "sandbox memory")
	expect(t, c, Warn, "could hold 30.0 GiB")
	expect(t, c, Warn, "above the host's 16.0 GiB")
	expect(t, c, Warn, "architect and librarian are counted once each")
	expect(t, c, Warn, "mason 4 × 4.0 GiB (memory)")
	if Failed(checks) {
		t.Fatalf("a memory warning failed doctor: %+v", checks)
	}
}

func TestSandboxMemoryWithinHostPasses(t *testing.T) {
	c := find(t, sizing(t, 32*gib, sizedRoles), "sandbox memory")
	expect(t, c, Pass, "within the host's 32.0 GiB")
	expect(t, c, Pass, "could hold 30.0 GiB")
}

func TestUnsizedRolesCountAtSbxDefault(t *testing.T) {
	roles := "[roles.mason]\nsandbox = \"sbx\"\n[roles.reviewer]\nsandbox = \"sbx\"\n"
	// Half of 16 GiB each: 4 masons and 2 reviewers hold 48 GiB.
	c := find(t, sizing(t, 16*gib, roles), "sandbox memory")
	expect(t, c, Warn, "could hold 48.0 GiB")
	expect(t, c, Warn, "mason 4 × 8.0 GiB (sbx default)")
	expect(t, c, Warn, "half the host's memory")
	if c.Remediation == "" || !contains(c.Remediation, "[roles.mason]", "[roles.reviewer]", "memory") {
		t.Fatalf("remediation = %q", c.Remediation)
	}
}

func TestSbxDefaultIsClamped(t *testing.T) {
	for _, c := range []struct {
		total, want uint64
	}{
		{256 << 20, 512 << 20},
		{1 * gib, 512 << 20},
		{16 * gib, 8 * gib},
		{64 * gib, 32 * gib},
		{512 * gib, 32 * gib},
	} {
		if got := sbxDefaultMemory(c.total); got != c.want {
			t.Errorf("sbxDefaultMemory(%d) = %d, want %d", c.total, got, c.want)
		}
	}
	// One unsized architect on a 512 GiB host counts at the 32 GiB ceiling.
	checks := sizing(t, 512*gib, "[roles.architect]\nsandbox = \"sbx\"\n")
	expect(t, find(t, checks, "sandbox memory"), Pass, "architect 1 × 32.0 GiB (sbx default)")
}

func TestOnlySbxRolesCount(t *testing.T) {
	absent(t, sizing(t, 1*gib, ""), "sandbox memory")
	roles := "[roles.mason]\nsandbox = \"container\"\nimage = \"alpine\"\n[roles.reviewer]\nsandbox = \"none\"\n"
	absent(t, sizing(t, 1*gib, roles), "sandbox memory")
	// Only the sbx role is counted.
	c := find(t, sizing(t, 16*gib, roles+"[roles.librarian]\nsandbox = \"sbx\"\nmemory = \"1g\"\n"), "sandbox memory")
	expect(t, c, Pass, "could hold 1.0 GiB")
}

func TestUnreadableHostMemoryWarns(t *testing.T) {
	o, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf(topLevel, projectID)+sizedRoles), 0o600))
	o.TotalMemory = func() (uint64, error) { return 0, errors.New("no meminfo") }
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "sandbox memory"), Warn, "no meminfo")
	if Failed(checks) {
		t.Fatalf("an unreadable host memory failed doctor: %+v", checks)
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]uint64{"512m": 512 << 20, "4g": 4 * gib, "1m": 1 << 20} {
		if got, ok := parseMemory(in); !ok || got != want {
			t.Errorf("parseMemory(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "4", "0g", "-1g", "1.5g", "4G", "4k", "99999999999999999999g"} {
		if _, ok := parseMemory(in); ok {
			t.Errorf("parseMemory(%q) accepted", in)
		}
	}
}

func contains(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
