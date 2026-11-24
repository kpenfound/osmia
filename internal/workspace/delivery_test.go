package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type signedDelivery struct {
	f        fixture
	base     string
	head     string
	key      string
	messages []DeliveryMessage
	squash   bool
	signed   string
}

func signDeliveryOf(t *testing.T, squash bool) *signedDelivery {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "commit", "--allow-empty", "-m", "First unit")
	first := git(t, "-C", f.clone, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "commit", "--allow-empty", "-m", "Second unit")
	head := git(t, "-C", f.clone, "rev-parse", "HEAD")
	key := filepath.Join(t.TempDir(), "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	for k, v := range map[string]string{"user.name": "Owner Name", "user.email": "owner@example.invalid", "user.signingkey": key, "gpg.format": "ssh", "commit.gpgsign": "false"} {
		git(t, "-C", f.clone, "config", k, v)
	}
	message := "Owner edited subject\n\nDetails.\n\nCo-authored-by: Claude <noreply@anthropic.com>\nCo-authored-by: Claude Shannon <human@example.invalid>"
	messages := []DeliveryMessage{{Commit: first, Message: message}, {Commit: head, Message: "Second message"}}
	if squash {
		messages = []DeliveryMessage{{Commit: head, Message: message}}
	}
	signed, err := f.provider.SignDelivery(ctx, "operation", base, head, messages, squash)
	if err != nil {
		t.Fatal(err)
	}
	return &signedDelivery{f: f, base: base, head: head, key: key, messages: messages, squash: squash, signed: signed}
}

func TestSignDeliveryMakesVerifiableCommitsInTheOwnersIdentity(t *testing.T) {
	for _, squash := range []bool{true, false} {
		t.Run(map[bool]string{true: "squash", false: "units"}[squash], func(t *testing.T) {
			t.Parallel()
			d := signDeliveryOf(t, squash)
			f := d.f
			if d.signed == d.head {
				t.Fatal("unsigned candidate was reused")
			}
			public, err := os.ReadFile(d.key + ".pub")
			if err != nil {
				t.Fatal(err)
			}
			allowed := filepath.Join(t.TempDir(), "allowed-signers")
			if err := os.WriteFile(allowed, append([]byte("owner@example.invalid "), public...), 0600); err != nil {
				t.Fatal(err)
			}
			commits := strings.Fields(git(t, "-C", f.clone, "rev-list", "--reverse", d.base+".."+d.signed))
			if len(commits) != len(d.messages) {
				t.Fatalf("signed %d commits, want %d", len(commits), len(d.messages))
			}
			for _, id := range commits {
				git(t, "-C", f.clone, "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", id)
				identity := git(t, "-C", f.clone, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", id)
				if identity != "Owner Name <owner@example.invalid>\nOwner Name <owner@example.invalid>" {
					t.Fatalf("identity: %s", identity)
				}
				msg := git(t, "-C", f.clone, "show", "-s", "--format=%B", id)
				if strings.Contains(msg, "noreply@anthropic.com") || strings.Count(msg, "Signed-off-by: Owner Name <owner@example.invalid>") != 1 {
					t.Fatalf("message: %s", msg)
				}
			}
			firstMessage := git(t, "-C", f.clone, "show", "-s", "--format=%B", commits[0])
			if !strings.Contains(firstMessage, "Co-authored-by: Claude Shannon <human@example.invalid>") {
				t.Fatal("human coauthor removed")
			}
		})
	}
}

func TestSignDeliveryKeepsTheTreeAndTheLocalBranch(t *testing.T) {
	t.Parallel()
	d := signDeliveryOf(t, false)
	if git(t, "-C", d.f.clone, "rev-parse", d.head+"^{tree}") != git(t, "-C", d.f.clone, "rev-parse", d.signed+"^{tree}") {
		t.Fatal("tree changed")
	}
	if got := git(t, "-C", d.f.clone, "rev-parse", "HEAD"); got != d.head {
		t.Fatal("local branch moved")
	}
}

func TestSignDeliveryReturnsTheSignedCandidateOfTheSameOperationAfterTheKeyIsGone(t *testing.T) {
	for _, squash := range []bool{true, false} {
		t.Run(map[bool]string{true: "squash", false: "units"}[squash], func(t *testing.T) {
			t.Parallel()
			d := signDeliveryOf(t, squash)
			ctx := context.Background()
			if err := os.Remove(d.key); err != nil {
				t.Fatal(err)
			}
			again, err := d.f.provider.SignDelivery(ctx, "operation", d.base, d.head, d.messages, d.squash)
			if err != nil || again != d.signed {
				t.Fatalf("recovery: %s %v", again, err)
			}
			if _, err := d.f.provider.SignDelivery(ctx, "new-operation", d.base, d.head, d.messages, d.squash); err == nil {
				t.Fatal("missing key signed a new delivery")
			}
		})
	}
}

func TestDeliverySigningProgramFailureBlocksPublication(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "commit", "--allow-empty", "-m", "Feature")
	head := git(t, "-C", f.clone, "rev-parse", "HEAD")
	key := filepath.Join(t.TempDir(), "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	failing := filepath.Join(t.TempDir(), "failing-ssh-keygen")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"user.name": "Owner Name", "user.email": "owner@example.invalid", "user.signingkey": key, "gpg.format": "ssh", "commit.gpgsign": "false", "gpg.ssh.program": failing} {
		git(t, "-C", f.clone, "config", k, v)
	}
	messages := []DeliveryMessage{{Commit: head, Message: "Feature"}}
	if _, err := f.provider.SignDelivery(ctx, "operation", base, head, messages, true); err == nil {
		t.Fatal("failing signing program signed a commit")
	}
	if refs := git(t, "-C", f.clone, "for-each-ref", "refs/core-sign"); refs != "" {
		t.Fatalf("failing signing program left a commit ref: %s", refs)
	}
	git(t, "-C", f.clone, "config", "gpg.ssh.program", "ssh-keygen")
	signed, err := f.provider.SignDelivery(ctx, "operation", base, head, messages, true)
	if err != nil {
		t.Fatalf("retry after fixing the signing program: %v", err)
	}
	if signed == head {
		t.Fatal("retry reused the unsigned candidate")
	}
}

func TestDeliveryMessageCleanup(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Subject\n\nCo-authored-by: Claude <noreply@anthropic.com>\nCo-authored-by: Human <human@example.com>", "Subject\n\nCo-authored-by: Human <human@example.com>"},
		{"Subject\n\nco-AUTHORED-by: Codex <bot@example.com>", "Subject"},
		{"Subject\n\nCo-authored-by: Claude <human@example.com>", "Subject\n\nCo-authored-by: Claude <human@example.com>"},
		{"Subject\n\nExample:\n Co-authored-by: Claude <noreply@anthropic.com>\n\nExplanation.", "Subject\n\nExample:\n Co-authored-by: Claude <noreply@anthropic.com>\n\nExplanation."},
	} {
		if got := CleanDeliveryMessage(tc.in); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestDeliveryRequiresOwnerConfiguration(t *testing.T) {
	for _, missing := range []string{"user.name", "user.email", "user.signingkey"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			base := git(t, "-C", f.clone, "rev-parse", "HEAD")
			git(t, "-C", f.clone, "commit", "--allow-empty", "-m", "Feature")
			head := git(t, "-C", f.clone, "rev-parse", "HEAD")
			for k, v := range map[string]string{"user.name": "Owner", "user.email": "owner@example.invalid", "user.signingkey": "unused"} {
				if k == missing {
					v = ""
				}
				git(t, "-C", f.clone, "config", k, v)
			}
			_, err := f.provider.SignDelivery(context.Background(), "op", base, head, []DeliveryMessage{{Commit: head, Message: "Feature"}}, true)
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("missing %s: %v", missing, err)
			}
			if refs := git(t, "-C", f.clone, "for-each-ref", "refs/core-sign"); refs != "" {
				t.Fatal("missing identity recorded delivery")
			}
		})
	}
}
