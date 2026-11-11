package config

import (
	"regexp"
	"strings"
)

// githubUser is a GitHub user account name: 1 to 39 letters, digits and
// single hyphens, neither starting nor ending with a hyphen.
var githubUser = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)

// ProjectCreation is the top-level [projects] table. RepoOwner is the GitHub
// user account new repositories are created under, which must be the account
// of GITHUB_TOKEN. CloneDir is the absolute directory new clones are made in;
// a leading ~/ is expanded when the configuration loads. Project creation is
// on only while both are set.
type ProjectCreation struct {
	RepoOwner string `toml:"repo_owner" json:"repo_owner,omitempty"`
	CloneDir  string `toml:"clone_dir" json:"clone_dir,omitempty"`
}

// CreationEnabled reports whether both keys are set, which is what turns
// project creation on.
func (p ProjectCreation) CreationEnabled() bool { return p.RepoOwner != "" && p.CloneDir != "" }

// MissingSetting names the first [projects] key that is not set, or is empty
// when creation is enabled.
func (p ProjectCreation) MissingSetting() string {
	switch {
	case p.RepoOwner == "":
		return "projects.repo_owner"
	case p.CloneDir == "":
		return "projects.clone_dir"
	}
	return ""
}

// validate checks both keys and resolves CloneDir. An unset key is valid;
// creation simply stays off.
func (p *ProjectCreation) validate(path string, root Root, home string) error {
	if p.RepoOwner != "" && (len(p.RepoOwner) > 39 || !githubUser.MatchString(p.RepoOwner)) {
		return fieldError(path, "projects.repo_owner", "must be a GitHub user account name: up to 39 letters, digits and single hyphens")
	}
	if p.CloneDir == "" {
		return nil
	}
	if !strings.HasPrefix(p.CloneDir, "~/") && p.CloneDir != "~" && !strings.HasPrefix(p.CloneDir, "/") {
		return fieldError(path, "projects.clone_dir", "must be an absolute directory, or start with ~/")
	}
	resolved, err := resolvePath(p.CloneDir, home, "")
	if err != nil {
		return fieldError(path, "projects.clone_dir", err.Error())
	}
	if root.Overlaps(resolved) {
		return fieldError(path, "projects.clone_dir", "clone directory and Osmia root must be separate, non-nested directories")
	}
	p.CloneDir = resolved
	return nil
}
