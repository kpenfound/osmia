package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

const (
	mebibyte = uint64(1) << 20
	gibibyte = uint64(1) << 30

	// sbx sizes a sandbox that sets no memory at half the host's memory,
	// clamped to this range.
	sbxMinimumMemory = 512 * mebibyte
	sbxMaximumMemory = 32 * gibibyte
)

// sbxDefaultMemory is the memory sbx gives a sandbox created without --memory
// on a host with total bytes of memory.
func sbxDefaultMemory(total uint64) uint64 {
	return min(max(total/2, sbxMinimumMemory), sbxMaximumMemory)
}

// parseMemory reads a validated sbx memory limit such as 512m or 4g.
func parseMemory(s string) (uint64, bool) {
	unit := mebibyte
	switch {
	case strings.HasSuffix(s, "g"):
		unit = gibibyte
	case !strings.HasSuffix(s, "m"):
		return 0, false
	}
	n, err := strconv.ParseUint(s[:len(s)-1], 10, 64)
	if err != nil || n == 0 || n > (1<<62)/unit {
		return 0, false
	}
	return n * unit, true
}

// formatMemory is bytes in GiB with one decimal.
func formatMemory(b uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/float64(gibibyte))
}

// sizedRole is one sbx role in the estimate: how many sandboxes of it can be
// alive at once and how much memory each may hold.
type sizedRole struct {
	name   string
	slots  int
	memory uint64
	sized  bool
}

// sandboxMemory estimates the host memory the configured sbx sandboxes could
// hold at once and warns when it exceeds the host's total memory. The
// mason, reviewer and committee pools count at their capacity; the architect
// and librarian have no factory-wide limit and run one turn per workstream, so
// each counts once, as do the chief of staff and foreman when they run in sbx.
// A role without memory counts at sbx's default.
func (r *run) sandboxMemory(cfg *config.Config) {
	if cfg == nil {
		return
	}
	var roles []sizedRole
	for _, p := range []struct {
		name  string
		slots int
	}{
		{"mason", cfg.Capacity.Masons}, {"reviewer", cfg.Capacity.Reviewers}, {"committee", cfg.Capacity.Committee},
		{"architect", 1}, {"librarian", 1}, {"chief_of_staff", 1}, {"foreman", 1},
	} {
		if role := cfg.Roles[p.name]; role.Sandbox == "sbx" {
			roles = append(roles, sizedRole{name: p.name, slots: p.slots, sized: role.Memory != ""})
			if memory, ok := parseMemory(role.Memory); ok {
				roles[len(roles)-1].memory = memory
			}
		}
	}
	if len(roles) == 0 {
		return
	}
	const name = "sandbox memory"
	total, err := r.TotalMemory()
	if err != nil || total == 0 {
		why := "no value"
		if err != nil {
			why = reason(err)
		}
		r.add(GroupConfig, name, Warn, "cannot read the host's total memory ("+why+"), so the memory the sbx sandboxes could hold is not compared with it", "check the roles' sizes by hand: set memory in [roles.<role>] so the sandboxes the capacity allows fit the host")
		return
	}
	var sum uint64
	var parts, unsized []string
	for _, role := range roles {
		memory, origin := role.memory, "memory"
		if !role.sized {
			memory, origin = sbxDefaultMemory(total), "sbx default"
			unsized = append(unsized, "[roles."+role.name+"]")
		}
		sum += uint64(role.slots) * memory
		parts = append(parts, fmt.Sprintf("%s %d × %s (%s)", role.name, role.slots, formatMemory(memory), origin))
	}
	detail := fmt.Sprintf("the sbx sandboxes could hold %s at once: %s. Bounded pools count at their capacity; architect and librarian are counted once each (each runs at most one turn per workstream, so more workstreams can hold more), and so are the chief of staff and foreman when they run in sbx. A role without memory counts at sbx's default of half the host's memory, between 512 MiB and 32 GiB", formatMemory(sum), strings.Join(parts, ", "))
	if sum <= total {
		r.add(GroupConfig, name, Pass, fmt.Sprintf("%s, within the host's %s of memory", detail, formatMemory(total)), "")
		return
	}
	remedy := "set memory in [roles.<role>], such as memory = \"4g\" for the mason and \"2g\" for read-only roles, or lower the capacity of the bounded pools"
	if len(unsized) > 0 {
		remedy = "set memory in " + strings.Join(unsized, ", ") + ", such as memory = \"4g\" for the mason and \"2g\" for read-only roles, or lower the capacity of the bounded pools"
	}
	r.add(GroupConfig, name, Warn, fmt.Sprintf("%s, above the host's %s of memory", detail, formatMemory(total)), remedy)
}

// hostMemory is the host's total memory in bytes: /proc/meminfo on Linux and
// sysctl hw.memsize on macOS.
func hostMemory() (uint64, error) {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		defer f.Close()
		scan := bufio.NewScanner(f)
		for scan.Scan() {
			fields := strings.Fields(scan.Text())
			if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024, err
			}
		}
		return 0, errors.New("/proc/meminfo has no MemTotal")
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	}
	return 0, fmt.Errorf("not supported on %s", runtime.GOOS)
}
