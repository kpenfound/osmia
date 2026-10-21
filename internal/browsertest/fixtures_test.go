package browsertest

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type page struct{ *Page }

func openBrowser(t *testing.T) *page { t.Helper(); return &page{OpenBrowser(t)} }

var quote = Quote
var textOf = TextOf
var clickCurrent = ClickCurrent
var newLinkProxy = NewLinkProxy

const browserTimeout = BrowserTimeout

var must = fixturekit.Must
var projectFixture = servicetest.ProjectFixture
var activityFixture = servicetest.ActivityFixture
var start = servicetest.Start
var mutation = servicetest.Mutation
var writeCharter = servicetest.WriteCharter
var withWeb = servicetest.WithWeb
var newDesignEnforcementFixture = servicetest.NewDesignEnforcementFixture
var sessionWebFixture = servicetest.SessionWebFixture
var librarianWorkstream = service.LibrarianWorkstreamForTest
var assertFeedAnnotations = func(t *testing.T, p *page, s string, n int) { AssertFeedAnnotations(t, p.Page, s, n) }

const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"
const quiet = fixturekit.Quiet
const otherProject = fixturekit.OtherProject
const otherStream = fixturekit.OtherStream

var ownerActor = fixturekit.OwnerActor

const draftBriefToolName = "draft_brief"

func sessionWorktreePath(root string, p config.ProjectID, s config.WorkstreamID) string {
	return filepath.Join(root, "sessions", string(p), string(s))
}

type pageFixture struct {
	*servicetest.PageFixture
	s     *service.Service
	c     *service.Client
	chief coreadapter.Scope
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	f := servicetest.NewPageFixture(t)
	return &pageFixture{PageFixture: f, s: f.S, c: f.C, chief: f.Chief}
}
func (f *pageFixture) status(t *testing.T, c trace.StatusContent) { t.Helper(); f.Status(t, c) }
func (f *pageFixture) answerChief(t *testing.T, w config.WorkstreamID, turn, final string) {
	t.Helper()
	fixturekit.AnswerChief(t, service.InspectForTest(f.s).SoleRepository(), w, turn, final)
}

type twoProjectFixture struct {
	opts    service.Options
	cfg     *config.Config
	streams map[config.ProjectID]config.WorkstreamID
}

func newTwoProjectFixture(t *testing.T) *twoProjectFixture {
	t.Helper()
	f := servicetest.NewTwoProjectFixture(t)
	return &twoProjectFixture{opts: f.Options, cfg: f.Config, streams: f.Streams}
}

const AbandonedState = service.AbandonedState
const DeliveredState = service.DeliveredState
const ActivityCheck = service.ActivityCheck

type CharterEdit = service.CharterEdit
type FinalReport = service.FinalReport
type CheckSelection = service.CheckSelection

const Prefix = service.Prefix

type HandInRequest = service.HandInRequest
type ProjectAddRequest = service.ProjectAddRequest
type RepoAddRequest = service.RepoAddRequest
type RuntimeResponse = service.RuntimeResponse
type PauseRequest = service.PauseRequest
type ClearPauseRequest = service.ClearPauseRequest
type SessionOpenRequest = service.SessionOpenRequest

const InboxEscalation = service.InboxEscalation
const TurnQueued = service.TurnQueued
const TurnDone = service.TurnDone

type ConversationResponse = service.ConversationResponse
type Client = service.Client
type Enforcement = service.Enforcement
type FinalCheckRun = service.FinalCheckRun
type FinalCheckAttempt = service.FinalCheckAttempt
type Options = service.Options
type Service = service.Service

const ChecksPassed = service.ChecksPassed
const ChecksIncomplete = service.ChecksIncomplete
const ChecksSuperseded = service.ChecksSuperseded

type UnitCheckRun = service.UnitCheckRun

var recordActivity = fixturekit.RecordActivity

const validSpec = fixturekit.ValidSpec
const validPlan = fixturekit.ValidPlan
const demoRole = fixturekit.DemoRole

var demoStart = fixturekit.DemoStart
var callTool = fixturekit.CallTool
var demoGit = fixturekit.DemoGit
var request = fixturekit.Request
var claimTurn = fixturekit.ClaimTurn

const project = fixturekit.Project
const stream = fixturekit.Stream

// entriesOf returns the listed inbox entries of one kind.
func entriesOf(t *testing.T, c *Client, kind string) []InboxEntry {
	t.Helper()
	inbox, err := c.Inbox(context.Background())
	must(t, err)
	var out []InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// decisionCard is the selector of the page's card of an inbox entry.
func decisionCard(e InboxEntry) string {
	return fmt.Sprintf(`[data-decision="%s"] `, strings.Join([]string{e.Kind, string(e.Project), string(e.Workstream), strconv.Itoa(e.Number), e.Unit, e.Amendment}, ":"))
}

var pauseOf = fixturekit.PauseOf

type InboxEntry = api.InboxEntry
type completedRunner = fixturekit.CompletedRunner

var sessionBranch = service.SessionBranchForTest

func awaitFeedEvent(p *page, ws config.WorkstreamID) { AwaitFeedEvent(p.Page, ws) }

var readFeed = servicetest.ReadFeed
var awaitDesignConversation = servicetest.AwaitDesignConversation
