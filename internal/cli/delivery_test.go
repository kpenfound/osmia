package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/workspace"
)

func TestApproveCommitMessageFiles(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(map[bool]string{false: "message", true: "messages"}[multiple], func(t *testing.T) {
			opts := fixture(t)
			root := opts.Config.Root
			ln, err := net.Listen("unix", filepath.Join(root, "fake.sock"))
			must(t, err)
			messages := []workspace.DeliveryMessage{{Commit: "abc", Message: "Draft"}}
			if multiple {
				messages = append(messages, workspace.DeliveryMessage{Commit: "def", Message: "Second draft"})
			}
			sent := make(chan service.DeliveryDecision, 1)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(service.DeliveryPresentation{Report: service.FinalReport{Review: 1, Commit: "abc"}, ReviewRevision: 2, DraftHash: "hash", Draft: "Presented description", Messages: messages})
				} else {
					var decision service.DeliveryDecision
					if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
						http.Error(w, err.Error(), 400)
						return
					}
					sent <- decision
					json.NewEncoder(w).Encode(service.DeliveryApproval{Review: 1, Commit: "abc", Messages: decision.Messages})
				}
			})}
			go server.Serve(ln)
			t.Cleanup(func() { server.Close() })
			path := filepath.Join(root, "message.txt")
			flag := "--message-file"
			content := []byte("Owner message\n\nDetails.\n")
			if multiple {
				flag = "--messages-file"
				messages[0].Message = "Owner message"
				content, err = json.Marshal(messages)
				must(t, err)
			}
			must(t, os.WriteFile(path, content, 0600))
			code, _, diag := invoke(t, root, "approve", stream, "--socket", "fake.sock", flag, path)
			if code != 0 {
				t.Fatalf("approve: %d %s", code, diag)
			}
			decision := <-sent
			if decision.Description == nil || *decision.Description != "Presented description" {
				t.Fatalf("description was not pinned: %+v", decision)
			}
			if len(decision.Messages) != len(messages) || decision.Messages[0].Commit != "abc" || decision.ReviewRevision != 2 || decision.DraftHash != "hash" {
				t.Fatalf("decision: %+v", decision)
			}
			want := string(content)
			if multiple {
				want = "Owner message"
			}
			if decision.Messages[0].Message != want {
				t.Fatalf("message: %q", decision.Messages[0].Message)
			}
		})
	}
}
