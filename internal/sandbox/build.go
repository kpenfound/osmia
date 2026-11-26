package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Build builds the osmia:latest template in a temporary sandbox of its own
// and saves it only when every verification passes: sbx and dagger are on
// PATH; the host's Dagger release is read, installed in the template and
// verified; Docker Engine is removed; Codex is at least CodexMinVersion;
// Claude Code is installed; and a probe finds no Docker Engine as root and as
// the agent user. A refused build, or a failed save, returns the reason and
// leaves the template saved before in place. The only sandbox it creates,
// stops or removes is its own temporary one, so sandboxes running from an
// earlier template are not touched.
func (h Host) Build(ctx context.Context) (err error) {
	if _, err := h.resolve("sbx"); err != nil {
		return err
	}
	if _, err := h.resolve("dagger"); err != nil {
		return fmt.Errorf("%w; the template installs the host's Dagger CLI release", err)
	}
	hostVersion, err := h.run(ctx, "", "dagger", "version")
	if err != nil {
		return fmt.Errorf("could not read the host Dagger version: %w", err)
	}
	daggerRelease := versionField(hostVersion)
	if daggerRelease == "" {
		return errors.New("could not read the host Dagger version from: dagger version")
	}
	daggerVersion := strings.TrimPrefix(daggerRelease, "v")

	name := uniqueName("osmia-build-")
	h.say("Building template %s in temporary sandbox %s.", Template, name)
	_, createErr := h.sbx(ctx, "", "create", "--name", name, "--skills", "off", "--pull", "always", "codex")
	defer func() {
		if rmErr := h.removeTemporary(ctx, name); rmErr != nil && createErr == nil {
			if err == nil {
				err = rmErr
			} else {
				err = fmt.Errorf("%w; %w", err, rmErr)
			}
		}
	}()
	if createErr != nil {
		return fmt.Errorf("could not create the build sandbox: %w", createErr)
	}

	// The codex template can carry Docker Engine and the Docker CLI. The
	// template keeps neither, so they go before anything is installed.
	if _, err := h.sbx(ctx, GuestRemoval(), "exec", "-i", "-u", "root", name, "sh", "-s", "osmia-docker-removal"); err != nil {
		return fmt.Errorf("could not remove Docker Engine from the build sandbox: %w", err)
	}
	// The codex template ships Codex; Claude Code comes from its native
	// installer.
	if _, err := h.sbx(ctx, ClaudeInstall(), "exec", "-i", name, "bash", "-eu"); err != nil {
		return fmt.Errorf("could not install Claude Code: %w", err)
	}
	if _, err := h.sbx(ctx, DaggerInstall(), "exec", "-i", "-u", "root", name,
		"env", "DAGGER_VERSION="+daggerVersion, "BIN_DIR=/usr/local/bin", "CI=1", "sh", "-eu"); err != nil {
		return fmt.Errorf("could not install the Dagger CLI: %w", err)
	}

	codexOutput, err := h.sbx(ctx, "", "exec", name, "codex", "--version")
	if err != nil {
		return fmt.Errorf("could not read the Codex version: %w", err)
	}
	h.say("%s", strings.TrimSpace(codexOutput))
	codex := firstVersion(codexOutput)
	if codex == "" {
		return fmt.Errorf("could not read the Codex version from: codex --version; %s or later is required, the first release that supports skills/extraRoots/set; refusing to save template", CodexMinVersion)
	}
	if !atLeast(codex, CodexMinVersion) {
		return fmt.Errorf("the Codex release %s is older than %s, the first release that supports skills/extraRoots/set; refusing to save template", codex, CodexMinVersion)
	}
	claudeOutput, err := h.sbx(ctx, "", "exec", name, "claude", "--version")
	if err != nil {
		return fmt.Errorf("the build sandbox has no usable Claude Code: %w", err)
	}
	h.say("%s", strings.TrimSpace(claudeOutput))
	guestDagger, err := h.sbx(ctx, "", "exec", name, "dagger", "version")
	if err != nil {
		return fmt.Errorf("could not read the Dagger version in the build sandbox: %w", err)
	}
	h.say("%s", strings.TrimSpace(guestDagger))
	if versionField(guestDagger) != "v"+daggerVersion {
		return fmt.Errorf("expected Dagger v%s; refusing to save template", daggerVersion)
	}

	// Both the root and the agent user are probed: DOCKER_HOST and socket
	// access differ between them.
	problems := h.proveDockerFree(ctx, name, "root", "-u", "root")
	problems = append(problems, h.proveDockerFree(ctx, name, "agent")...)
	if len(problems) > 0 {
		return fmt.Errorf("refusing to save template %s; it must have no Docker Engine: %s", Template, strings.Join(problems, "; "))
	}
	h.say("No Docker Engine in the build sandbox: no dockerd or containerd process and no reachable daemon.")

	if _, err := h.sbx(ctx, "", "stop", name); err != nil {
		return fmt.Errorf("could not stop the build sandbox before saving: %w", err)
	}
	if _, err := h.sbx(ctx, "", "template", "save", name, Template); err != nil {
		return fmt.Errorf("could not save template %s; the template saved before is unchanged: %w", Template, err)
	}
	h.say("Saved local template %s with Dagger v%s.", Template, daggerVersion)
	return nil
}

// proveDockerFree runs the guest probe with the given sbx exec options and
// returns every reason to refuse. A probe that fails to run, prints something
// unexpected or leaves a required probe unanswered is a refusal.
func (h Host) proveDockerFree(ctx context.Context, name, label string, options ...string) []string {
	args := append([]string{"exec", "-i"}, options...)
	args = append(args, name, "bash", "-s", "osmia-docker-probe")
	out, err := h.sbx(ctx, GuestProbe(), args...)
	if err != nil {
		return []string{fmt.Sprintf("as %s: the probe could not run (sbx exec failed: %v)", label, err)}
	}
	var problems []string
	seen := map[string]bool{}
	done := false
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		status, rest, _ := strings.Cut(line, " ")
		probe, _, _ := strings.Cut(rest, ":")
		switch {
		case status == "ok" && isProbe(probe):
			seen[probe] = true
		case (status == "found" || status == "unknown") && isProbe(probe):
			seen[probe] = true
			problems = append(problems, fmt.Sprintf("as %s: %s", label, line))
		case line == "done":
			done = true
		default:
			problems = append(problems, fmt.Sprintf("as %s: unexpected probe output: %s", label, line))
		}
	}
	for _, probe := range []string{"processes", "binaries", "endpoints"} {
		if !seen[probe] {
			problems = append(problems, fmt.Sprintf("as %s: the %s probe gave no answer", label, probe))
		}
	}
	if !done {
		problems = append(problems, fmt.Sprintf("as %s: the probe did not run to completion", label))
	}
	return problems
}

func isProbe(name string) bool {
	return name == "processes" || name == "binaries" || name == "endpoints"
}
