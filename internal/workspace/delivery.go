package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// DeliveryMessage pairs an original revision with its owner-approved message.
type DeliveryMessage struct {
	Commit  string `json:"commit"`
	Message string `json:"message"`
}

// DeliveryMessages reads the linear unit history in landing order.
func (g *Git) DeliveryMessages(ctx context.Context, base, head string) ([]DeliveryMessage, error) {
	out, err := g.run(ctx, "rev-list", "--reverse", base+".."+head)
	if err != nil {
		return nil, err
	}
	var messages []DeliveryMessage
	parent := base
	for _, id := range strings.Fields(out) {
		c, err := g.Commit(ctx, id)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(c.Parents, []string{parent}) {
			return nil, errors.New("delivery requires a linear history on the reviewed base")
		}
		messages = append(messages, DeliveryMessage{Commit: id, Message: c.Message})
		parent = id
	}
	if parent != head || len(messages) == 0 {
		return nil, errors.New("delivery has no commits on the reviewed base")
	}
	return messages, nil
}

var agentCoauthor = regexp.MustCompile(`(?i)^(claude( code| (opus|sonnet|haiku)( [0-9.]+)?)|codex|chatgpt|github copilot|copilot|gemini|cursor|aider|osmia)\s*<`)

// CleanDeliveryMessage removes agent co-author trailers, retaining human
// attribution and prose, including quoted examples of trailers.
func CleanDeliveryMessage(message string) string {
	lines := strings.Split(strings.TrimSpace(message), "\n")
	// Only the final paragraph can be the trailer block.
	start := len(lines) - 1
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	var kept []string
	for i, line := range lines {
		key, value, ok := strings.Cut(line, ":")
		if i >= start && ok && strings.EqualFold(strings.TrimSpace(key), "Co-authored-by") {
			v := strings.TrimSpace(value)
			lower := strings.ToLower(v)
			if agentCoauthor.MatchString(v) || strings.Contains(lower, "<noreply@anthropic.com>") || strings.Contains(lower, "<noreply@openai.com>") {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// TODO: Remove the signed-commit construction and recovery-ref adapter when
// busybees/core provides service-owned signed history rewriting.
//
// SignDelivery writes owner-attributed, signed commits without moving the
// working branch. The operation's ref retains the completed result so recovery
// reuses it even if recording the publication was interrupted.
func (g *Git) SignDelivery(ctx context.Context, operation, base, head string, messages []DeliveryMessage, squash bool, at time.Time) (string, error) {
	original, err := g.Commit(ctx, head)
	if err != nil {
		return "", err
	}
	if squash {
		if len(messages) != 1 || messages[0].Commit != head {
			return "", errors.New("squash message differs from the approved candidate")
		}
		ok, err := g.Ancestor(ctx, base, head)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("delivery candidate does not descend from reviewed base")
		}
	} else {
		expected, err := g.DeliveryMessages(ctx, base, head)
		if err != nil {
			return "", err
		}
		if len(expected) != len(messages) {
			return "", errors.New("delivery messages differ from the reviewed history")
		}
		for i := range expected {
			if expected[i].Commit != messages[i].Commit {
				return "", errors.New("delivery message names an unreviewed commit")
			}
		}
	}
	for _, m := range messages {
		if strings.TrimSpace(CleanDeliveryMessage(m.Message)) == "" || strings.ContainsRune(m.Message, 0) {
			return "", errors.New("delivery commit message must not be empty or contain NUL")
		}
	}
	intent, _ := json.Marshal(struct {
		Operation, Base, Head string
		Messages              []DeliveryMessage
		Squash                bool
	}{operation, base, head, messages, squash})
	ref := fmt.Sprintf("refs/osmia/delivery/%x", sha256.Sum256(intent))
	saved, err := g.run(ctx, "rev-parse", "--verify", "--quiet", ref)
	if err == nil {
		c, err := g.Commit(ctx, saved)
		if err != nil {
			return "", err
		}
		if c.Tree != original.Tree {
			return "", errors.New("recorded signed delivery differs from reviewed tree")
		}
		return saved, nil
	}
	if !exitCode(err, 1) {
		return "", err
	}
	value := func(key string) (string, error) {
		v, err := g.run(ctx, "config", "--get", key)
		if err != nil || strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("delivery requires Git %s", key)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", fmt.Errorf("invalid Git %s", key)
		}
		return v, nil
	}
	name, err := value("user.name")
	if err != nil {
		return "", err
	}
	email, err := value("user.email")
	if err != nil {
		return "", err
	}
	key, err := value("user.signingkey")
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(name+email, "<>") {
		return "", errors.New("invalid delivery identity")
	}
	date := fmt.Sprintf("@%d +0000", at.Unix())
	env := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email, "GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	for _, k := range []string{"GNUPGHOME", "GPG_TTY"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	parent := base
	for _, m := range messages {
		c, err := g.Commit(ctx, m.Commit)
		if err != nil {
			return "", err
		}
		message := CleanDeliveryMessage(m.Message)
		trailer := "Signed-off-by: " + name + " <" + email + ">"
		signedOff, err := g.runInput(ctx, g.Clone, nil, strings.NewReader(message+"\n"), "-c", "trailer.ifexists=addIfDifferent", "-c", "trailer.ifmissing=add", "interpret-trailers", "--trailer", trailer)
		if err != nil {
			return "", err
		}
		id, err := g.runInput(ctx, g.Clone, env, strings.NewReader(signedOff), "commit-tree", "-S"+key, "-p", parent, "-F", "-", c.Tree)
		if err != nil {
			return "", fmt.Errorf("sign delivery commit: %w", err)
		}
		parent = strings.TrimSpace(id)
	}
	if _, err := g.run(ctx, "update-ref", ref, parent, strings.Repeat("0", len(parent))); err != nil {
		return "", err
	}
	return parent, nil
}
