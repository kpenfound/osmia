package config

import (
	"fmt"
	"slices"
	"strings"
)

// The error sources of [error_reports] sources.
const (
	ErrorSourceAPI       = "api"
	ErrorSourceOperation = "operation"
	ErrorSourceGitHub    = "github"
)

// ErrorSources lists every error source the reporter can file.
var ErrorSources = []string{ErrorSourceAPI, ErrorSourceOperation, ErrorSourceGitHub}

// The defaults of [error_reports].
const (
	DefaultErrorReportsRepository = "kpenfound/osmia"
	DefaultMaxIssuesPerHour       = 5
	DefaultMaxIssuesPerDay        = 20
	DefaultMaxCommentsPerDay      = 20
)

// ErrorReports configures the error reporter, which files GitHub issues about
// the service's own failures. Repository is the owner/name repository the
// issues go to and Labels are put on each new issue. The caps bound new issues
// per hour and per day and comments per day. Sources lists the error sources
// that are filed: all of them unless the file narrows it.
type ErrorReports struct {
	Enabled           bool     `toml:"enabled" json:"enabled"`
	Repository        string   `toml:"repository" json:"repository"`
	Labels            []string `toml:"labels" json:"labels"`
	MaxIssuesPerHour  int      `toml:"max_issues_per_hour" json:"max_issues_per_hour"`
	MaxIssuesPerDay   int      `toml:"max_issues_per_day" json:"max_issues_per_day"`
	MaxCommentsPerDay int      `toml:"max_comments_per_day" json:"max_comments_per_day"`
	Sources           []string `toml:"sources" json:"sources"`
}

func defaultErrorReports() ErrorReports {
	return ErrorReports{
		Repository:        DefaultErrorReportsRepository,
		Labels:            []string{"factory-error", "automated"},
		MaxIssuesPerHour:  DefaultMaxIssuesPerHour,
		MaxIssuesPerDay:   DefaultMaxIssuesPerDay,
		MaxCommentsPerDay: DefaultMaxCommentsPerDay,
		Sources:           slices.Clone(ErrorSources),
	}
}

// SourceEnabled reports whether errors of source are filed.
func (e ErrorReports) SourceEnabled(source string) bool { return slices.Contains(e.Sources, source) }

func (e ErrorReports) validate(path string) error {
	if !ValidRepository(e.Repository) {
		return fieldError(path, "error_reports.repository", "must be an owner/name GitHub repository")
	}
	for i, label := range e.Labels {
		if strings.TrimSpace(label) == "" {
			return fieldError(path, fmt.Sprintf("error_reports.labels[%d]", i), "must not be empty")
		}
	}
	for i, source := range e.Sources {
		if !slices.Contains(ErrorSources, source) {
			return fieldError(path, fmt.Sprintf("error_reports.sources[%d]", i), "expected "+strings.Join(ErrorSources, ", "))
		}
	}
	for _, value := range []struct {
		field string
		n     int
	}{
		{"error_reports.max_issues_per_hour", e.MaxIssuesPerHour},
		{"error_reports.max_issues_per_day", e.MaxIssuesPerDay},
		{"error_reports.max_comments_per_day", e.MaxCommentsPerDay},
	} {
		if value.n <= 0 {
			return fieldError(path, value.field, "must be positive")
		}
	}
	return nil
}
