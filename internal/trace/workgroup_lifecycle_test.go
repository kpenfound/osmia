package trace

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// This checks the binding and journal interface. Service lifecycle acceptance
// additionally requires the production design, ratification and archive path.
func TestCreatedWorkgroupSharesAuthorityBeforeCreatorCloses(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	members := []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}
	created, group, err := CreateWorkgroupTrace(context.Background(), root, members, at, owner, "bindings")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenWorkgroupTrace(root, group.ID, members)
	if err != nil {
		created.Close()
		t.Fatal(err)
	}
	defer opened.Close()
	if created != opened {
		created.Close()
		t.Fatal("creator and reader have different authorities")
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	groups, _, err := Workgroups(opened)
	if err != nil || len(groups) != 1 || !reflect.DeepEqual(groups[0], group) {
		t.Fatalf("closing creator invalidated reader: %+v %v", groups, err)
	}
	if _, err := r1.Workflow(crossMember1, FeatureSubject); err != nil {
		t.Fatalf("group took ownership of member handle: %v", err)
	}
}

func TestWorkgroupBindingsObserveRetirementOnOverlappingHandles(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	members := []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}
	created, group, err := CreateWorkgroupTrace(context.Background(), root, members, at, owner, "bindings")
	if err != nil {
		t.Fatal(err)
	}
	pending := GroupApproval{Workgroup: group.ID, Members: []GroupMemberApproval{
		{Project: crossProject1, Workstream: crossMember1, Spec: 1, Plan: 2, Round: 3, Record: []byte(`{"pin":"first"}`)},
		{Project: crossProject2, Workstream: crossMember2, Spec: 4, Plan: 5, Round: 6, Record: []byte(`{"pin":"second"}`)},
	}}
	if err := RecordGroupApproval(context.Background(), created, group.Members[0], pending, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	open := func() *Repository {
		t.Helper()
		r, err := OpenWorkgroupTrace(root, group.ID, members)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first, second := open(), open()
	firstOpen, secondOpen := true, true
	t.Cleanup(func() {
		if firstOpen {
			first.Close()
		}
	})
	t.Cleanup(func() {
		if secondOpen {
			second.Close()
		}
	})
	if first != second {
		t.Fatal("overlapping handles have different authority")
	}
	var retired atomic.Bool
	if err := r1.SetWorkgroupMemberLifecycle(func(id config.WorkgroupID, stream config.WorkstreamID) (bool, error) {
		if id != group.ID || stream != crossMember1 {
			return false, ErrConflict
		}
		return retired.Load(), nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func(stage string, r *Repository) {
		t.Helper()
		decision, doc, found, err := ReadGroupApproval(r, group.Members[0])
		if err != nil || !found || doc.Revision != 1 || !reflect.DeepEqual(decision, pending) || decision.Complete {
			t.Fatalf("%s pending pins changed: %+v revision=%d found=%v err=%v", stage, decision, doc.Revision, found, err)
		}
		if _, err := r.Workflow(crossMember2, FeatureSubject); err != nil {
			t.Fatalf("%s survivor: %v", stage, err)
		}
		if _, err := r.Workflow(crossMember1, FeatureSubject); !errors.Is(err, ErrMemberRetired) {
			t.Fatalf("%s unavailable side effect must hold: %v", stage, err)
		}
		groups, _, err := Workgroups(r)
		if err != nil || len(groups) != 1 || !reflect.DeepEqual(groups[0], group) {
			t.Fatalf("%s historical membership changed: %+v %v", stage, groups, err)
		}
	}
	retired.Store(true)
	check("retained first", first)
	check("retained second", second)
	if moved, err := r1.MarkWorkstreamTraceForDeletion(crossMember1); err != nil || !moved {
		t.Fatalf("mark deletion: %v %v", moved, err)
	}
	if err := FinishWorkstreamTraceDeletion(root, crossProject1, crossMember1); err != nil {
		t.Fatal(err)
	}
	check("deleted first", first)
	check("deleted second", second)
	third := open()
	check("deleted new", third)
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	authority, err := OpenWorkgroupAuthority(root, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("authority", authority)
	if authority != first {
		t.Fatal("journal access created another authority")
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if gone, err := WorkstreamTraceGone(root, crossProject1, crossMember1); err != nil || !gone {
		t.Fatalf("retired trace revived: %v %v", gone, err)
	}
	shared := sharedWorkgroupTraceFor(group.ID)
	shared.Lock()
	refs := shared.refs
	shared.Unlock()
	if refs != 2 {
		t.Fatalf("overlapping reference ownership: %d, want 2", refs)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	firstOpen = false
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	secondOpen = false
	// A cold journal-only recovery has no member bindings and needs no
	// deleted trace or cached live handle to retain the approved pins.
	recovered, err := OpenWorkgroupAuthority(root, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	decision, doc, found, err := ReadGroupApproval(recovered, group.Members[0])
	if err != nil || !found || doc.Revision != 1 || !reflect.DeepEqual(decision, pending) || decision.Complete {
		t.Fatalf("cold journal recovery changed unapplied approval: %+v found=%v err=%v", decision, found, err)
	}
	if len(recovered.memberBindings()) != 0 {
		t.Fatal("journal recovery opened member history")
	}
	if gone, err := WorkstreamTraceGone(root, crossProject1, crossMember1); err != nil || !gone {
		t.Fatalf("cold journal access revived history: %v %v", gone, err)
	}
}

func TestWorkgroupBindingFailureDoesNotProveRetirement(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	members := []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}
	created, group, err := CreateWorkgroupTrace(context.Background(), root, members, at, owner, "bindings")
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	long, err := OpenWorkgroupTrace(root, group.ID, members)
	if err != nil {
		t.Fatal(err)
	}
	defer long.Close()
	failure := errors.New("archive identity cannot be verified")
	if err := r1.SetWorkgroupMemberLifecycle(func(config.WorkgroupID, config.WorkstreamID) (bool, error) {
		return false, failure
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := long.Workflow(crossMember1, FeatureSubject); !errors.Is(err, failure) || errors.Is(err, ErrMemberRetired) {
		t.Fatalf("lifecycle error masked as retirement: %v", err)
	}
	if reopened, err := OpenWorkgroupTrace(root, group.ID, members); !errors.Is(err, failure) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatalf("open masked lifecycle evidence failure: %v", err)
	}
	if err := r1.SetWorkgroupMemberLifecycle(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.MarkWorkstreamTraceForDeletion(crossMember1); err != nil {
		t.Fatal(err)
	}
	if _, err := long.Workflow(crossMember1, FeatureSubject); err == nil || errors.Is(err, ErrMemberRetired) {
		t.Fatalf("unexplained absence masked as retirement: %v", err)
	}
	authority, err := OpenWorkgroupAuthority(root, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if groups, _, err := Workgroups(authority); err != nil || len(groups) != 1 {
		t.Fatalf("journal authority lost because member prerequisite failed: %+v %v", groups, err)
	}
}

// This exercises the routing interface's partial-commit recovery separately
// from the service's owner archive/cleanup and live-controller acceptance.
func TestRetiredFirstOriginPreservesPartialRulingAndHoldsRemainingRelay(t *testing.T) {
	ctx := context.Background()
	root, r1, r2, _, _ := crossRepoFixture(t)
	wg, g := createCrossRepoWorkgroup(t, root, r1, r2)
	worker1 := claimCrossRepoTurn(t, r1, crossProject1, crossMember1, "mason1", "mason", "", "build1")
	first, err := r1.Ask(ctx, "mason1", worker1, "Does the original contract apply?", at)
	if err != nil {
		t.Fatal(err)
	}
	worker2 := claimCrossRepoTurn(t, r2, crossProject2, crossMember2, "mason2", "mason", "", "build2")
	second, err := r2.Ask(ctx, "mason2", worker2, "Does the same contract apply here?", at)
	if err != nil {
		t.Fatal(err)
	}
	chief := claimCrossRepoTurn(t, wg, wg.Project(), g.Members[0], ChiefOfStaff, ChiefOfStaff, "", "chief1")
	_, err = wg.EscalateQuestions(ctx, ChiefOfStaff, chief, EscalationRequest{Questions: []string{string(crossMember1) + ":" + first.ID, string(crossMember2) + ":" + second.ID}, Rephrasing: "One decision for both origins.", Blocked: "Both origins.", Recommendation: "Keep the contract."}, at)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := wg.Inbox()
	if err != nil || len(inbox) != 1 {
		t.Fatalf("original inbox: %+v %v", inbox, err)
	}
	number := inbox[0].Number
	stop := errors.New("interrupted after first ruling commit")
	ruleInterrupt = func(config.WorkstreamID) error { return stop }
	t.Cleanup(func() { ruleInterrupt = nil })
	if _, err := wg.Rule(ctx, number, "Keep the contract.", owner, at); !errors.Is(err, stop) {
		t.Fatalf("interruption: %v", err)
	}
	ruleInterrupt = nil
	original, err := Get[Ruling](r1, crossMember1, first.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := wg.PreserveMemberQuestions(ctx, g, CrossRepoMember{Workstream: crossMember1, Project: r1}, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := r1.SetWorkgroupMemberLifecycle(func(group config.WorkgroupID, stream config.WorkstreamID) (bool, error) {
		if group != g.ID || stream != crossMember1 {
			return false, ErrConflict
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.MarkWorkstreamTraceForDeletion(crossMember1); err != nil {
		t.Fatal(err)
	}
	if err := FinishWorkstreamTraceDeletion(root, crossProject1, crossMember1); err != nil {
		t.Fatal(err)
	}
	inbox, err = wg.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].Workstream != crossMember1 || inbox[0].State != QuestionEscalated || len(inbox[0].Questions) != 2 || inbox[0].Questions[0].Asked.ID != first.ID {
		t.Fatalf("retired first locator lost partial authority: %+v %v", inbox, err)
	}
	if _, err := wg.Rule(ctx, number, "A different decision.", owner, at); !errors.Is(err, ErrRuled) {
		t.Fatalf("different ruling accepted: %v", err)
	}
	if _, err := wg.Rule(ctx, number, "Keep the contract.", owner, at); err != nil {
		t.Fatal(err)
	}
	pinned, err := wg.QuestionRuling(crossMember1, first.ID, 1)
	if err != nil || !reflect.DeepEqual(pinned, original) {
		t.Fatalf("original owner attribution or pins lost: %+v %v", pinned, err)
	}
	for range 2 {
		if _, err := wg.RelayRuling(ctx, ChiefOfStaff, chief, string(crossMember1)+":"+first.ID, "Keep the contract.", ScopeLocal, at); !errors.Is(err, ErrMemberRetired) {
			t.Fatalf("unsatisfied retired relay did not remain held: %v", err)
		}
		qs, err := r2.Questions(crossMember2)
		if err != nil || len(qs) != 1 || qs[0].State != QuestionAnswered || qs[0].Ruling == nil || qs[0].Ruling.Revision != 2 || qs[0].Asked.ID != second.ID || qs[0].Asked.Turn != "build2" {
			t.Fatalf("eligible survivor did not progress exactly once: %+v %v", qs, err)
		}
	}
	inbox, err = wg.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].Hold == "" || inbox[0].Questions[0].State != QuestionRuled {
		t.Fatalf("remaining retired prerequisite was discarded: %+v %v", inbox, err)
	}
	if gone, err := WorkstreamTraceGone(root, crossProject1, crossMember1); err != nil || !gone {
		t.Fatalf("retired history revived: %v %v", gone, err)
	}
}

func TestSharedAggregatesObserveRetirementDuringEnumeration(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	group, _ := createCrossRepoWorkgroup(t, root, r1, r2)
	for _, read := range []struct {
		name string
		run  func(*testing.T) error
	}{
		{"status", func(t *testing.T) error {
			statuses, err := group.Statuses()
			if err == nil && (len(statuses) != 1 || statuses[0].Workstream != crossMember2) {
				t.Fatalf("live aggregate included a retired member: %+v", statuses)
			}
			return err
		}},
		{"charter", func(t *testing.T) error { _, err := group.CharterProposals(); return err }},
		{"records", func(t *testing.T) error {
			_, streams, err := group.scanAll()
			if err == nil && !reflect.DeepEqual(streams, []config.WorkstreamID{crossMember2}) {
				t.Fatalf("record aggregate included retired history: %v", streams)
			}
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			var reads atomic.Int32
			if err := r1.SetWorkgroupMemberLifecycle(func(config.WorkgroupID, config.WorkstreamID) (bool, error) {
				// Enumeration sees live state; scoped resolution sees the
				// subsequent durable retirement through the same binding.
				return reads.Add(1) > 1, nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := read.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSharedAggregateErrorDoesNotEstablishRetirement(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	group, _ := createCrossRepoWorkgroup(t, root, r1, r2)
	var reads atomic.Int32
	if err := r1.SetWorkgroupMemberLifecycle(func(config.WorkgroupID, config.WorkstreamID) (bool, error) {
		if reads.Add(1) == 1 {
			return false, nil
		}
		return false, ErrMemberRetired
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := group.Statuses(); !errors.Is(err, ErrMemberRetired) {
		t.Fatalf("an error without terminal evidence hid a live member: %v", err)
	}
}

func TestRetirementPreservesQuestionRoutedToPendingAmendment(t *testing.T) {
	ctx := context.Background()
	root, r1, r2, _, _ := crossRepoFixture(t)
	group, g := createCrossRepoWorkgroup(t, root, r1, r2)
	setBuilding(t, r2, crossMember2, crossProject2, "resume")
	worker := claimCrossRepoTurn(t, r2, crossProject2, crossMember2, "mason1", "mason", "resume", "build1")
	q, err := r2.Ask(ctx, "mason1", worker, "May I widen the retry window?", at)
	if err != nil {
		t.Fatal(err)
	}
	chief := claimCrossRepoTurn(t, group, group.Project(), g.Members[0], ChiefOfStaff, ChiefOfStaff, "", "chief1")
	if _, err := group.FileAmendment(ctx, ChiefOfStaff, chief, AmendmentRequest{
		QuestionID: string(crossMember2) + ":" + q.ID, Citations: []string{"plan#resume"},
		Change: "Widen the retry window", Reason: "The reviewer found a gap",
		Seal: 1, SealRevision: 1, SpecHash: "sha256:test",
	}, at); err != nil {
		t.Fatal(err)
	}
	before, err := r2.Questions(crossMember2)
	if err != nil || len(before) != 1 || before[0].State != QuestionRouted {
		t.Fatalf("pending amendment origin: %+v %v", before, err)
	}
	if err := group.PreserveMemberQuestions(ctx, g, CrossRepoMember{Workstream: crossMember2, Project: r2}, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := r2.SetWorkgroupMemberLifecycle(func(config.WorkgroupID, config.WorkstreamID) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	preserved, err := group.retiredQuestions(crossMember2)
	if err != nil || !reflect.DeepEqual(preserved, before) || preserved[0].Ruling != nil {
		t.Fatalf("retirement changed or completed a routed question: %+v %v", preserved, err)
	}
}

func TestSharedQuestionReadClosedChildWithoutRetirementEvidence(t *testing.T) {
	for _, read := range []string{"questions", "inbox", "ruling", "snapshot"} {
		t.Run(read, func(t *testing.T) {
			root, r1, r2, _, _ := crossRepoFixture(t)
			group, _ := createCrossRepoWorkgroup(t, root, r1, r2)
			child, err := r1.scoped(crossMember1)
			if err != nil {
				t.Fatal(err)
			}
			var closed atomic.Bool
			if err := group.SetQuestionReadBoundary(func(stream config.WorkstreamID) error {
				if stream == crossMember1 && closed.CompareAndSwap(false, true) {
					return child.closeFiles()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			defer group.SetQuestionReadBoundary(nil)
			switch read {
			case "questions":
				_, _, err = group.QuestionActions(crossMember1)
			case "inbox":
				_, err = group.Inbox()
			case "ruling":
				_, err = group.QuestionRuling(crossMember1, "1", 1)
			case "snapshot":
				err = group.WithMemberQuestionSnapshot(crossMember1, func(MemberQuestionSnapshot) error {
					t.Fatal("unproved closed history produced a snapshot")
					return nil
				})
			}
			if !closed.Load() || err == nil || errors.Is(err, ErrMemberRetired) {
				t.Fatalf("closed child without archive evidence was hidden as retirement: %v", err)
			}
		})
	}
}

func TestSharedWorkflowReadClosedChildWithoutRetirementEvidence(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	group, _ := createCrossRepoWorkgroup(t, root, r1, r2)
	child, err := r1.scoped(crossMember1)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.closeFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := group.WorkflowStates(crossMember1); err == nil || errors.Is(err, ErrMemberRetired) {
		t.Fatalf("closed workflow history without archive evidence was hidden as retirement: %v", err)
	}
}
