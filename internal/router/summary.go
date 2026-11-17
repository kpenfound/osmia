package router

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// The bounds of the summary Jev reads. The request as a whole stays under
// jev.MaxDecisionBytes; the summary gives way first.
const (
	// maxEntryText bounds one record's text in the summary.
	maxEntryText = 4 << 10
	// maxHistory is how many of the subject's latest transitions the summary
	// shows.
	maxHistory = 10
	// maxReason bounds a history entry's reason.
	maxReason = 300
	// requestMargin is the room the summary leaves in the request for the
	// framing the questions and options add.
	requestMargin = 8 << 10
)

// summary is the structured state Jev reads for one exit. Everything in it is
// a record's own text or a word, count or duration computed by code; none of
// it is code.
type summary struct {
	Task           string          `json:"task"`
	Table          string          `json:"table"`
	Trigger        string          `json:"trigger"`
	Machine        string          `json:"machine"`
	Subject        string          `json:"subject"`
	State          string          `json:"state"`
	MinutesInState int             `json:"minutes_in_state"`
	Default        string          `json:"default"`
	Targets        []summaryTarget `json:"targets"`
	Intent         string          `json:"intent,omitempty"`
	Facts          map[string]any  `json:"facts,omitempty"`
	Evidence       []Entry         `json:"evidence,omitempty"`
	History        []historyEntry  `json:"history,omitempty"`
	Truncated      bool            `json:"truncated,omitempty"`
}

type summaryTarget struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Bar is the probability the target needs for a departure: "default" for
	// the default itself, "modest" or "strict".
	Bar string `json:"bar"`
}

type historyEntry struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Reason     string `json:"reason"`
	DecidedBy  string `json:"decided_by,omitempty"`
	MinutesAgo int    `json:"minutes_ago"`
}

var fence = regexp.MustCompile("(?s)```.*?(```|$)")

// plain returns text without fenced code, capped at limit bytes at a rune
// boundary.
func plain(text string, limit int) string {
	text = strings.TrimSpace(fence.ReplaceAllString(text, "[code omitted]"))
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func minutes(from, to time.Time) int {
	if from.IsZero() || to.Before(from) {
		return 0
	}
	return int(to.Sub(from) / time.Minute)
}

// buildSummary builds the summary of the exit for the legal targets. now is the
// time the minutes are measured to; transitions are the workstream's, read
// from the trace.
func buildSummary(x Exit, legal []Target, transitions []trace.Transition, now time.Time) summary {
	s := summary{Task: jev.TransitionTask, Table: TableVersion, Trigger: x.Trigger, Machine: x.Machine, Subject: x.Subject, State: x.From, Default: x.Default,
		Intent: plain(x.Evidence.Intent, maxEntryText)}
	for _, t := range legal {
		bar := "default"
		if t.Name != x.Default {
			bar = string(BarModest)
			if t.Bar == BarStrict {
				bar = string(BarStrict)
			}
		}
		s.Targets = append(s.Targets, summaryTarget{Name: t.Name, Description: plain(t.Description, 500), Bar: bar})
	}
	if len(x.Evidence.Facts) > 0 {
		s.Facts = map[string]any{}
		for k, v := range x.Evidence.Facts {
			switch val := v.(type) {
			case string:
				s.Facts[k] = plain(val, 200)
			case bool, int, int64, uint64, float64, float32, uint, int32:
				s.Facts[k] = val
			}
		}
	}
	for _, e := range x.Evidence.Records {
		e.Text = plain(e.Text, maxEntryText)
		s.Evidence = append(s.Evidence, e)
	}
	var mine []trace.Transition
	for _, t := range transitions {
		if t.Subject == x.Subject {
			mine = append(mine, t)
		}
	}
	if n := len(mine); n > 0 {
		s.MinutesInState = minutes(mine[n-1].At, now)
	}
	if len(mine) > maxHistory {
		mine = mine[len(mine)-maxHistory:]
	}
	for _, t := range mine {
		s.History = append(s.History, historyEntry{From: t.From, To: t.To, Reason: plain(t.Reason, maxReason), DecidedBy: t.DecidedBy, MinutesAgo: minutes(t.At, now)})
	}
	return s
}

// fit trims the summary until its JSON is within limit bytes: older history
// first, then the evidence, longest text first. It reports the JSON.
func (s *summary) fit(limit int) ([]byte, error) {
	for {
		data, err := json.Marshal(s)
		if err != nil || len(data) <= limit {
			return data, err
		}
		s.Truncated = true
		switch {
		case len(s.History) > 0:
			s.History = s.History[1:]
		case len(s.Evidence) > 0:
			longest := 0
			for i, e := range s.Evidence {
				if len(e.Text) > len(s.Evidence[longest].Text) {
					longest = i
				}
			}
			if t := s.Evidence[longest].Text; len(t) > 256 {
				s.Evidence[longest].Text = plain(t, len(t)/2)
			} else {
				s.Evidence = s.Evidence[:len(s.Evidence)-1]
			}
		default:
			return data, nil
		}
	}
}

// summaryBudget is how many bytes of summary fit in a request whose options
// are given: the options appear in the summary's target list and in both pick
// questions.
func summaryBudget(options []jev.TransitionOption) int {
	used := requestMargin
	for _, o := range options {
		used += 3*(len(o.Description)+len(o.Name)) + 64
	}
	return max(jev.MaxDecisionBytes-used, 4<<10)
}
