package sandbox

import (
	"context"
	"fmt"
)

// Check reports whether the template saved now exists and carries a Claude
// Code of ClaudeMinVersion or later, the first release that runs Fable 5.2. It
// asks the saved template itself: it creates a short-lived sandbox from it,
// reads `claude --version` there and removes that sandbox. It keeps no
// evidence, so a template replaced by a build on either path, a removed
// template or a failed build attempt is judged on what is saved at the time.
func (h Host) Check(ctx context.Context) error {
	if _, err := h.resolve("sbx"); err != nil {
		return err
	}
	name := uniqueName("osmia-check-")
	_, createErr := h.sbx(ctx, "", "create", "--no-progress", "--name", name, "--skills", "off", "--template", Template, "claude")
	defer func() {
		rmErr := h.removeTemporary(ctx, name)
		if rmErr != nil && createErr == nil {
			h.say("warning: %v", rmErr)
		}
	}()
	if createErr != nil {
		return fmt.Errorf("template %s is not usable; it must be saved with osmia sandbox build: %w", Template, createErr)
	}
	out, err := h.sbx(ctx, "", "exec", name, "claude", "--version")
	if err != nil {
		return fmt.Errorf("could not read Claude Code's version in template %s: %w", Template, err)
	}
	version := firstVersion(out)
	if version == "" {
		return fmt.Errorf("could not read Claude Code's version in template %s from: claude --version", Template)
	}
	if !atLeast(version, ClaudeMinVersion) {
		return fmt.Errorf("template %s has Claude Code %s, older than %s, the first release that runs Fable 5.2; rebuild it with osmia sandbox build", Template, version, ClaudeMinVersion)
	}
	h.say("Template %s has Claude Code %s, which runs Fable 5.2 (%s or later).", Template, version, ClaudeMinVersion)
	return nil
}
