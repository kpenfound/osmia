package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/vcs"
	coregit "github.com/kpenfound/agentcore/vcs/git"
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

func deliveryIntentHash(operation, base, head string, messages []DeliveryMessage, squash bool) string {
	intent, _ := json.Marshal(struct {
		Operation, Base, Head string
		Messages              []DeliveryMessage
		Squash                bool
	}{operation, base, head, messages, squash})
	return fmt.Sprintf("%x", sha256.Sum256(intent))
}

// SignDelivery makes owner-attributed, signed commits of the reviewed
// history through core's commit signer (core/vcs/git.Signer), which uses the
// clone's configured identity, signing key and signing format and never
// returns a commit it could not sign. Osmia checks the messages against the
// reviewed linear history, removes agent co-author trailers, adds the
// owner's Signed-off-by trailer, and derives a durable signing ID from the
// publication operation and the approved intent, so a retry finds the same
// signing rather than making one again.
func (g *Git) SignDelivery(ctx context.Context, operation, base, head string, messages []DeliveryMessage, squash bool) (string, error) {
	if _, err := g.Commit(ctx, head); err != nil {
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
	hash := deliveryIntentHash(operation, base, head, messages, squash)
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
	if _, err := value("user.signingkey"); err != nil {
		return "", err
	}
	if strings.ContainsAny(name+email, "<>") {
		return "", errors.New("invalid delivery identity")
	}
	trailer := "Signed-off-by: " + name + " <" + email + ">"
	req := vcs.SignRequest{ID: hash}
	for i, m := range messages {
		c, err := g.Commit(ctx, m.Commit)
		if err != nil {
			return "", err
		}
		signedOff, err := g.runInput(ctx, g.Clone, nil, strings.NewReader(CleanDeliveryMessage(m.Message)+"\n"), "-c", "trailer.ifexists=addIfDifferent", "-c", "trailer.ifmissing=add", "interpret-trailers", "--trailer", trailer)
		if err != nil {
			return "", err
		}
		spec := vcs.CommitSpec{Tree: c.Tree, Message: signedOff}
		if i == 0 {
			spec.Parents = []string{base}
		} else {
			spec.FollowsPrevious = true
		}
		req.Commits = append(req.Commits, spec)
	}
	signing, err := (coregit.Signer{}).Sign(ctx, vcs.Directory(g.Clone), req)
	if err != nil {
		return "", fmt.Errorf("sign delivery commit: %w", err)
	}
	return signing.Head, nil
}
