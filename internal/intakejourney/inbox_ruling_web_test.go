package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	questions "github.com/kpenfound/osmia/internal/questions"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProjectPaneWebRulingResumesBothOriginsOnce(t *testing.T) {
	t.Parallel()
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%v", interrupted), func(t *testing.T) {
			ctx := context.Background()
			f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
			t.Cleanup(func() { f.Stop(t) })
			f.Stop(t)
			intakefixture.WithWeb(t, f.Opts.Config.Root, "127.0.0.1:0")
			f.Start(t)
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			f.AwaitApproved(t)
			const answer = "Keep the acknowledged offset contract stable."
			var escalated, relay atomic.Bool
			f.Engine.Mu.Lock()
			f.Engine.Turns["web-ruling-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the acknowledged offset contract change?"})
				return intakefixture.QuestionResult(req, "web-worker", "Asked the chief."), err
			}
			f.Engine.Turns[questions.TurnID("1")] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
				return intakefixture.QuestionResult(req, "web-worker", "Continued the original asking turn."), nil
			}
			f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				if strings.Contains(req.Prompt, "Present the shared decision") && escalated.CompareAndSwap(false, true) {
					_, err := fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{
						"questions":  []string{string(f.Stream1) + ":1", string(f.Stream2) + ":1"},
						"rephrasing": "May the acknowledged offset contract change?", "blocked": "Both asking origins.",
						"options": []string{"Keep", "Change"}, "recommendation": "Keep the contract stable.",
					})
					return intakefixture.QuestionResult(req, "web-chief", "Asked the owner."), err
				}
				if strings.Contains(req.Prompt, "Relay the recorded decision") && relay.Load() {
					_, err := fixturekit.CallTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.Stream2) + ":1", "text": answer, "scope": "local"})
					return intakefixture.QuestionResult(req, "web-chief", "Relayed to both origins."), err
				}
				return intakefixture.QuestionResult(req, "web-chief", "Following the agreed contract."), nil
			}
			f.Engine.Mu.Unlock()
			f.SharedDesign().LifecycleWorker(t, f.Project, f.Stream1, "worker_first", "web-ruling-ask", "Check the contract.")
			f.SharedDesign().LifecycleWorker(t, f.Project2, f.Stream2, "worker_second", "web-ruling-ask", "Check the contract.")
			f.Wait(t, "both original questions", func() bool {
				for i, id := range []config.WorkstreamID{f.Stream1, f.Stream2} {
					project := f.Project
					if i == 1 {
						project = f.Project2
					}
					repo, err := f.S.Repository(project)
					fixturekit.Must(t, err)
					qs, err := repo.Questions(id)
					fixturekit.Must(t, err)
					if len(qs) != 1 {
						return false
					}
				}
				return true
			})
			_, err = f.C.Send(ctx, f.Stream2, "Present the shared decision")
			fixturekit.Must(t, err)
			var entry api.InboxEntry
			f.Wait(t, "one pinned shared entry", func() bool {
				inbox, err := f.C.Inbox(ctx)
				fixturekit.Must(t, err)
				if len(inbox.Entries) != 1 {
					return false
				}
				entry = inbox.Entries[0]
				return entry.Workgroup != "" && entry.Batch == "escalation_1" && len(entry.Asked) == 2
			})
			post := func() (int, []byte) {

				client := intakefixture.WebHTTP()
				defer client.CloseIdleConnections()
				base := "http://" + f.S.WebAddr()
				code, data := intakefixture.Exchange(t, client, "GET", base+api.Prefix+"/feed/"+string(f.Stream2), "", "", "")
				if code != 200 {
					t.Fatalf("second member feed: %d %s", code, data)
				}
				body := map[string]any{}
				for k, v := range entry.Answer.Body {
					body[k] = v
				}
				body["text"] = answer
				encoded, err := json.Marshal(body)
				fixturekit.Must(t, err)
				return intakefixture.Exchange(t, client, entry.Answer.Method, base+entry.Answer.Path, "application/json", string(encoded), "")
			}
			if interrupted {
				hook := &intakefixture.DesignInterruption{Step: "owner-ruling-member", Nth: 1, Reached: make(chan struct{})}
				f.Hook.Store(hook)
				type response struct {
					code int
					body []byte
				}
				finished := make(chan response, 1)
				go func() { code, body := post(); finished <- response{code, body} }()
				select {
				case <-hook.Reached:
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("web ruling did not commit the first member")
				}
				f.Stop(t)
				var result response
				select {
				case result = <-finished:
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("interrupted web ruling did not finish")
				}
				if result.code != 503 || intakefixture.ErrorCode(t, result.body) != api.Unavailable || !strings.Contains(string(result.body), "was interrupted; refresh the inbox and retry the same answer") {
					t.Fatalf("interrupted web ruling: %d %s", result.code, result.body)
				}
				f.Hook.Store(nil)
				f.Start(t)
				_, err = f.C.Answer(ctx, entry.Number, "Change the contract instead.", entry.Project)
				var apiErr *api.APIError
				if !errors.As(err, &apiErr) || apiErr.Code != api.Conflict || apiErr.Message != fmt.Sprintf("inbox entry %d is already answered", entry.Number) {
					t.Fatalf("different-text retry did not preserve the original ruling: %v", err)
				}
			}
			code, data := post()
			if code != 200 {
				t.Fatalf("shared web ruling: %d %s", code, data)
			}
			var response api.AnswerResponse
			fixturekit.Must(t, json.Unmarshal(data, &response))
			if response.Number != entry.Number || response.Workstream != entry.Workstream || response.Batch != entry.Batch || response.Ruling != answer || len(response.Questions) != 2 || response.Questions[0] != "1" || response.Questions[1] != "1" {
				t.Fatalf("answer lost the shared entry pins: %+v", response)
			}
			f.Wait(t, "shared entry removed from API inbox", func() bool {
				inbox, err := f.C.Inbox(ctx)
				fixturekit.Must(t, err)
				return len(inbox.Entries) == 0
			})
			relay.Store(true)
			_, err = f.C.Send(ctx, f.Stream2, "Relay the recorded decision")
			fixturekit.Must(t, err)
			f.Wait(t, "both origins complete once", func() bool {
				for i, id := range []config.WorkstreamID{f.Stream1, f.Stream2} {
					project, worker := f.Project, "worker_first"
					if i == 1 {
						project, worker = f.Project2, "worker_second"
					}
					repo, err := f.S.Repository(project)
					fixturekit.Must(t, err)
					qs, err := repo.Questions(id)
					fixturekit.Must(t, err)
					th, err := repo.Thread(id, worker)
					fixturekit.Must(t, err)
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
