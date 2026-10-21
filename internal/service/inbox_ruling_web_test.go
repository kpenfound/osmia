package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProjectPaneWebRulingResumesBothOriginsOnce(t *testing.T) {
	t.Parallel()
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%v", interrupted), func(t *testing.T) {
			ctx := context.Background()
			f := newProductionDesignFixture(t)
			t.Cleanup(func() { f.stop(t) })
			f.stop(t)
			withWeb(t, f.opts, "127.0.0.1:0")
			f.start(t)
			f.startOwner(t)
			packet := f.awaitPacket(t)
			_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			must(t, err)
			f.awaitApproved(t)
			const answer = "Keep the acknowledged offset contract stable."
			var escalated, relay atomic.Bool
			f.engine.Mu.Lock()
			f.engine.Turns["web-ruling-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the acknowledged offset contract change?"})
				return questionResult(req, "web-worker", "Asked the chief."), err
			}
			f.engine.Turns[questions.TurnID("1")] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
				return questionResult(req, "web-worker", "Continued the original asking turn."), nil
			}
			f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if strings.Contains(req.Prompt, "Present the shared decision") && escalated.CompareAndSwap(false, true) {
					_, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{
						"questions":  []string{string(f.stream1) + ":1", string(f.stream2) + ":1"},
						"rephrasing": "May the acknowledged offset contract change?", "blocked": "Both asking origins.",
						"options": []string{"Keep", "Change"}, "recommendation": "Keep the contract stable.",
					})
					return questionResult(req, "web-chief", "Asked the owner."), err
				}
				if strings.Contains(req.Prompt, "Relay the recorded decision") && relay.Load() {
					_, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.stream2) + ":1", "text": answer, "scope": "local"})
					return questionResult(req, "web-chief", "Relayed to both origins."), err
				}
				return questionResult(req, "web-chief", "Following the agreed contract."), nil
			}
			f.engine.Mu.Unlock()
			lifecycleWorker(t, f, f.project, f.stream1, "worker_first", "web-ruling-ask", "Check the contract.")
			lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "web-ruling-ask", "Check the contract.")
			f.wait(t, "both original questions", func() bool {
				for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
					project := f.project
					if i == 1 {
						project = f.project2
					}
					repo, err := f.s.repository(project)
					must(t, err)
					qs, err := repo.Questions(id)
					must(t, err)
					if len(qs) != 1 {
						return false
					}
				}
				return true
			})
			_, err = f.c.Send(ctx, f.stream2, "Present the shared decision")
			must(t, err)
			var entry InboxEntry
			f.wait(t, "one pinned shared entry", func() bool {
				inbox, err := f.c.Inbox(ctx)
				must(t, err)
				if len(inbox.Entries) != 1 {
					return false
				}
				entry = inbox.Entries[0]
				return entry.Workgroup != "" && entry.Batch == "escalation_1" && len(entry.Asked) == 2
			})
			post := func() (int, []byte) {
				// Read the second member's real feed, then use the entry's own path and
				// project pin, exactly as the decision in that drill-down does.
				client := webHTTP()
				defer client.CloseIdleConnections()
				base := "http://" + f.s.WebAddr()
				code, data := exchange(t, client, "GET", base+Prefix+"/feed/"+string(f.stream2), "", "", "")
				if code != 200 {
					t.Fatalf("second member feed: %d %s", code, data)
				}
				body := map[string]any{}
				for k, v := range entry.Answer.Body {
					body[k] = v
				}
				body["text"] = answer
				encoded, err := json.Marshal(body)
				must(t, err)
				return exchange(t, client, entry.Answer.Method, base+entry.Answer.Path, "application/json", string(encoded), "")
			}
			if interrupted {
				hook := &designInterruption{step: "owner-ruling-member", nth: 1, reached: make(chan struct{})}
				f.hook.Store(hook)
				type response struct {
					code int
					body []byte
				}
				finished := make(chan response, 1)
				go func() { code, body := post(); finished <- response{code, body} }()
				select {
				case <-hook.reached:
				case <-time.After(demoTimeout):
					t.Fatal("web ruling did not commit the first member")
				}
				f.stop(t)
				var result response
				select {
				case result = <-finished:
				case <-time.After(demoTimeout):
					t.Fatal("interrupted web ruling did not finish")
				}
				if result.code != 503 || errorCode(t, result.body) != Unavailable || !strings.Contains(string(result.body), "was interrupted; refresh the inbox and retry the same answer") {
					t.Fatalf("interrupted web ruling: %d %s", result.code, result.body)
				}
				f.hook.Store(nil)
				f.start(t)
				_, err = f.c.Answer(ctx, entry.Number, "Change the contract instead.", entry.Project)
				var api *APIError
				if !errors.As(err, &api) || api.Code != Conflict || api.Message != fmt.Sprintf("inbox entry %d is already answered", entry.Number) {
					t.Fatalf("different-text retry did not preserve the original ruling: %v", err)
				}
			}
			code, data := post()
			if code != 200 {
				t.Fatalf("shared web ruling: %d %s", code, data)
			}
			var response AnswerResponse
			must(t, json.Unmarshal(data, &response))
			if response.Number != entry.Number || response.Workstream != entry.Workstream || response.Batch != entry.Batch || response.Ruling != answer || len(response.Questions) != 2 || response.Questions[0] != "1" || response.Questions[1] != "1" {
				t.Fatalf("answer lost the shared entry pins: %+v", response)
			}
			f.wait(t, "shared entry removed from API inbox", func() bool {
				inbox, err := f.c.Inbox(ctx)
				must(t, err)
				return len(inbox.Entries) == 0
			})
			relay.Store(true)
			_, err = f.c.Send(ctx, f.stream2, "Relay the recorded decision")
			must(t, err)
			f.wait(t, "both origins complete once", func() bool {
				for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
					project, worker := f.project, "worker_first"
					if i == 1 {
						project, worker = f.project2, "worker_second"
					}
					repo, err := f.s.repository(project)
					must(t, err)
					qs, err := repo.Questions(id)
					must(t, err)
					th, err := repo.Thread(id, worker)
					must(t, err)
					count, complete := 0, false
					for _, turn := range th.Turns {
						if turn.Request.TurnID == questions.TurnID("1") {
							count++
							complete = turn.Response != nil && !turn.CompletedAt.IsZero() && strings.Contains(turn.Request.Prompt, answer)
						}
					}
					if count > 1 {
						t.Fatal("duplicate origin continuation")
					}
					if len(qs) != 1 || qs[0].State != trace.QuestionAnswered || qs[0].Ruling.OwnerResponse != answer || qs[0].Ruling.ReturnedAnswer != answer || count != 1 || !complete {
						return false
					}
				}
				return true
			})
		})
	}
}
