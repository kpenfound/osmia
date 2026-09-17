// Package questions carries a role's question to the chief of staff and the
// chief of staff's choice back: the ask tool, the chief of staff's question
// tools, citation checks and delivery of an answer to the asker's thread.
package questions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// CitationForms lists the citations an answer may rest on, for prompts and
// refusals.
const CitationForms = "charter#<n> (a charter rule), kb/<subsystem>.md (a knowledge-base file), ruling#<record> (a ruling recorded in this workstream), spec#<n> (an acceptance criterion of this workstream's spec) or plan#<unit> (a unit of this workstream's plan)"

var (
	charterCitation = regexp.MustCompile(`^charter#([1-9][0-9]{0,8})$`)
	proseCitation   = regexp.MustCompile(`^kb/([^/]+)\.md$`)
	rulingCitation  = regexp.MustCompile(`^ruling#([A-Za-z0-9][A-Za-z0-9_-]{0,127})$`)
	planCitation    = regexp.MustCompile(`^plan#(.+)$`)
)

// IsCharter reports whether citation has the form of a charter rule,
// charter#<n>, and IsProse the form of a knowledge-base file, kb/<subsystem>.md.
func IsCharter(citation string) bool { return charterCitation.MatchString(citation) }
func IsProse(citation string) bool   { return proseCitation.MatchString(citation) }

// Unresolved is why a citation cannot support an answer.
type Unresolved struct{ Citation, Reason string }

func (e *Unresolved) Error() string {
	return fmt.Sprintf("citation %q does not resolve: %s", e.Citation, e.Reason)
}

// Resolve checks that citation names something recorded for the workstream:
// a rule of the latest charter, an existing knowledge-base file, a ruling of
// the workstream, or a criterion or unit of the workstream's latest recorded
// spec or plan. It returns *Unresolved for a citation that names nothing or
// whose knowledge-base file or plan cannot be read, and any other error for a
// trace that cannot be read. Reading the charter first records an owner edit,
// as every charter read does.
func Resolve(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, citation string, now time.Time) error {
	unresolved := func(format string, args ...any) error {
		return &Unresolved{Citation: citation, Reason: fmt.Sprintf(format, args...)}
	}
	if m := charterCitation.FindStringSubmatch(citation); m != nil {
		doc, err := repository.Charter(ctx, now)
		if err != nil {
			return err
		}
		n, _ := strconv.Atoi(m[1])
		if _, ok := charter.Parse(doc.Content).Rule(n); !ok {
			return unresolved("the charter has no rule numbered %d exactly once", n)
		}
		return nil
	}
	if m := proseCitation.FindStringSubmatch(citation); m != nil {
		subsystems, err := repository.Subsystems()
		if err != nil {
			return err
		}
		if slices.Contains(subsystems, m[1]) {
			if _, err := repository.Prose(m[1]); err == nil {
				return nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return unresolved("%s cannot be read: %v", citation, err)
			}
		}
		return unresolved("the knowledge base has no file %s", citation)
	}
	if m := rulingCitation.FindStringSubmatch(citation); m != nil {
		rulings, err := trace.Read[trace.Ruling](repository, stream)
		if err != nil {
			return err
		}
		for _, r := range rulings {
			if r.ID == m[1] {
				return nil
			}
		}
		return unresolved("this workstream records no ruling %s", m[1])
	}
	if n, ok := plan.ParseCitation(citation); ok {
		content, ok, err := latestDocument(repository, stream, plan.SpecPath)
		if err != nil {
			return err
		}
		if !ok {
			return unresolved("this workstream has no recorded spec")
		}
		if _, ok := plan.ParseSpec(content).Criterion(n); !ok {
			return unresolved("the spec has no acceptance criterion numbered %d exactly once", n)
		}
		return nil
	}
	if m := planCitation.FindStringSubmatch(citation); m != nil {
		content, ok, err := latestDocument(repository, stream, plan.PlanPath)
		if err != nil {
			return err
		}
		if !ok {
			return unresolved("this workstream has no recorded plan")
		}
		p, err := plan.Parse([]byte(content))
		if err != nil {
			return unresolved("the recorded plan cannot be read: %v", err)
		}
		if _, ok := p.Unit(m[1]); !ok {
			return unresolved("the plan has no unit %s exactly once", m[1])
		}
		return nil
	}
	return unresolved("it is not one of %s", CitationForms)
}

// latestDocument returns the content of the newest recorded revision of the
// workstream document at path.
func latestDocument(repository *trace.Repository, stream config.WorkstreamID, path string) (string, bool, error) {
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", false, err
	}
	content, found := "", false
	for _, d := range documents {
		if d.Path == path {
			content, found = d.Content, true
		}
	}
	return content, found, nil
}
