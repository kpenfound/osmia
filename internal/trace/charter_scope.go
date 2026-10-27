package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
)

// MemberCharter labels a member project's charter and its workstream.
type MemberCharter struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Document   Document            `json:"document"`
}

// WorkgroupCharters reads every member charter, without acquiring another project's
// writer lock. The shared authority's bindings are preferred; directory discovery
// also supports member-only readers and an authority reopened without bindings.
func (r *Repository) WorkgroupCharters(ctx context.Context, stream config.WorkstreamID) ([]MemberCharter, error) {
	if r.parent != nil {
		return r.parent.WorkgroupCharters(ctx, stream)
	}
	authority := r
	if r.memberBindings() == nil && stream != "" {
		id, err := r.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return nil, err
		}
		if id != "" {
			authority, err = OpenWorkgroupAuthority(r.root, id)
			if err != nil {
				return nil, err
			}
			defer authority.Close()
		}
	}
	members, err := authority.WorkgroupMembers(stream)
	if err != nil {
		return nil, err
	}
	cross := authority != r || authority.shared != nil || authority.memberBindings() != nil
	if !cross {
		d, err := r.Charter(ctx)
		return []MemberCharter{{Project: r.Project(), Workstream: stream, Document: d}}, err
	}
	var out []MemberCharter
	for _, member := range members {
		if binding, ok := authority.memberBinding(member); ok {
			d, err := binding.Project.Charter(ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, MemberCharter{binding.Project.Project(), member, d})
			continue
		}
		// The first stream of a cross-project authority is its synthetic chief host.
		if member == members[0] {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(r.root.String(), "workstreams"))
		if err != nil {
			return nil, err
		}
		var project config.ProjectID
		for _, entry := range entries {
			id := config.ProjectID(entry.Name())
			if config.CheckProjectIDs(id) != nil {
				continue
			}
			dir, err := r.root.WorkstreamTrace(id, member)
			if err != nil {
				return nil, err
			}
			exists, err := HasTraceDatabase(dir)
			if err != nil {
				return nil, err
			}
			if exists {
				if project != "" {
					return nil, fmt.Errorf("ambiguous project for workstream %s", member)
				}
				project = id
			}
		}
		if project == "" {
			approval, _, found, err := ReadGroupApproval(authority, members[0])
			if err != nil {
				return nil, err
			}
			if found {
				for _, pin := range approval.Members {
					if pin.Workstream == member {
						project = pin.Project
					}
				}
			}
		}
		if project == "" {
			return nil, fmt.Errorf("no project for workgroup member %s", member)
		}
		dir, err := r.root.ProjectTrace(project)
		if err != nil {
			return nil, err
		}
		db, err := openReadOnly(dir)
		if err != nil {
			return nil, err
		}
		d, err := r.latestDocument(ctx, db, "", "charter.md")
		closeErr := db.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		out = append(out, MemberCharter{project, member, d})
	}
	return out, nil
}

// ResolveCharter resolves an unqualified rule against the citing member and a
// qualified rule against a member project. Unknown projects and rules are refused.
func (r *Repository) ResolveCharter(ctx context.Context, stream config.WorkstreamID, citation string) (MemberCharter, charter.Rule, error) {
	named, n, ok := charter.Citation(citation)
	if !ok {
		return MemberCharter{}, charter.Rule{}, fmt.Errorf("not a charter citation: %s", citation)
	}
	members, err := r.WorkgroupCharters(ctx, stream)
	if err != nil {
		return MemberCharter{}, charter.Rule{}, err
	}
	own := r.Project()
	for _, m := range members {
		if m.Workstream == stream {
			own = m.Project
			break
		}
	}
	if m, found := r.memberBinding(stream); found {
		own = m.Project.Project()
	}
	if named == "" && len(members) > 0 && !slices.ContainsFunc(members, func(m MemberCharter) bool { return m.Project == own }) {
		own = members[0].Project
	}
	for _, m := range members {
		if named != "" && string(m.Project) != named || named == "" && m.Project != own {
			continue
		}
		rule, ok := charter.Parse(m.Document.Content).Rule(n)
		if !ok {
			return MemberCharter{}, charter.Rule{}, fmt.Errorf("the charter has no rule numbered %d exactly once", n)
		}
		return m, rule, nil
	}
	return MemberCharter{}, charter.Rule{}, fmt.Errorf("the workgroup has no project %s", named)
}

// CheckCharterConflict requires rules from at least two member projects.
func (r *Repository) CheckCharterConflict(ctx context.Context, stream config.WorkstreamID, citations []string) error {
	projects := map[config.ProjectID]bool{}
	for _, c := range citations {
		m, _, err := r.ResolveCharter(ctx, stream, c)
		if err != nil {
			return err
		}
		projects[m.Project] = true
	}
	if len(projects) < 2 {
		return fmt.Errorf("a charter conflict must cite rules from two member projects")
	}
	return nil
}
