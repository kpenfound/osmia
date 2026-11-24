package web

import (
	"fmt"
	"strings"
	"testing"
)

const amendmentEntry = `{"kind":"amendment","project":"p_fake","workstream":"w_000","number":0,"unit":"","amendment":"1","revision":1,` +
	`"question":"Raise the budget?","options":["approve","reject","round"],"recommendation":"approve: no objection stands","quick_reply":"",` +
	`"opened_at":"2026-01-01T00:00:00Z","asked":[{"asked_by":"mason","unit":"index","question":"May the budget rise?"},{"asked_by":"reviewer","question":"Is the budget enough?"}],` +
	`"answer":{"method":"POST","path":"/v1/amendment/w_000/1","body":{"packet":3}}}`

func TestBrowserDecisionCardLeadsWithTheQuestionAndRecommendsOneOption(t *testing.T) {
	t.Parallel()
	f := newDecisionButtonFixture(t, amendmentEntry)
	card := `[data-decision][data-kind="amendment"] `
	assertDecisionButtons(f.p, card, map[string]string{"approve": "Approve", "reject": "Reject", "round": "Debate another round"})
	assertButtonStyles(f.p, card, "approve")
	var shown struct {
		First         string
		OptionsList   bool
		YellowBox     bool
		QuestionFirst bool
		PrimaryFill   string
		SecondaryFill string
	}
	f.p.eval(`(() => {
  const card = document.querySelector(`+quote(card)+`);
  const fill = (choice) => getComputedStyle(card.querySelector('[data-decision-choice="' + choice + '"]')).backgroundColor;
  return {
    first: card.firstElementChild.firstElementChild.dataset.field,
    optionsList: card.textContent.includes('Options:'),
    yellowBox: card.querySelector('.attention') !== null,
    questionFirst: card.querySelector('[data-field=question]') === card.querySelector('p, button, summary, textarea, input'),
    primaryFill: fill('approve'),
    secondaryFill: fill('reject'),
  };
})()`, &shown)
	if shown.First != "question" || !shown.QuestionFirst || shown.OptionsList || shown.YellowBox || shown.PrimaryFill == shown.SecondaryFill {
		t.Fatalf("the decision card: %+v", shown)
	}
	f.p.click(card + `[data-decision-choice="reject"]`)
	f.p.awaitGone("the decided card", card)
	assertDecisionRequest(t, f, "/v1/amendment/w_000/1", map[string]any{"packet": float64(3), "decision": "reject"})
}

func TestBrowserEscalationCardRecommendsTheQuickReply(t *testing.T) {
	t.Parallel()
	const entry = `{"kind":"escalation","project":"p_fake","workstream":"w_000","number":1,"question":"Where should state live?",` +
		`"options":["Files","A database"],"recommendation":"Files.","quick_reply":"Files","opened_at":"2026-01-01T00:00:00Z","asked":[],` +
		`"answer":{"method":"POST","path":"/v1/inbox/1","body":{"project":"p_fake"}}}`
	for _, tc := range []struct {
		name string
		do   func(f *decisionButtonFixture, card string)
		text string
	}{
		{"accept", func(f *decisionButtonFixture, card string) { f.p.click(card + "[data-field=accept]") }, "Files"},
		{"option then answer", func(f *decisionButtonFixture, card string) {
			f.p.click(card + "[data-field=option]:nth-of-type(2)")
			f.p.click(card + "button[type=submit]")
		}, "A database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionButtonFixture(t, entry)
			card := `[data-decision][data-kind="escalation"] `
			f.p.awaitVisible("the question", card+"[data-field=accept]")
			var got struct {
				First                   string
				Accept, Submit, Options []string
			}
			f.p.eval(`(() => {
  const card = document.querySelector(`+quote(card)+`);
  const classes = (selector) => [...card.querySelectorAll(selector)].map(b => [...b.classList].join(' '));
  return {first: card.firstElementChild.firstElementChild.dataset.field, accept: classes('[data-field=accept]'), submit: classes('button[type=submit]'), options: classes('[data-field=option]')};
})()`, &got)
			if got.First != "question" || strings.Join(got.Accept, "|") != "primary" || strings.Join(got.Submit, "|") != "secondary" || strings.Join(got.Options, "|") != "secondary|secondary" {
				t.Fatalf("the question card: %+v", got)
			}
			tc.do(f, card)
			f.p.awaitGone("the answered question", card)
			assertDecisionRequest(t, f, "/v1/inbox/1", map[string]any{"project": "p_fake", "text": tc.text})
		})
	}
}

const amendmentCard = `[data-decision][data-kind="amendment"] `

func openFootnotesPage(t *testing.T) *page {
	t.Helper()
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) {
		api.setInbox(amendmentEntry)
		api.setFeed("w_000", routedTransitionEntry("2026-01-01T00:00:01Z", "ratified", "service:jev", "jev", "building", true))
	})
	f.p.awaitVisible("the amendment", amendmentCard+"details.footnote.revisions")
	return f.p
}

func TestBrowserDecisionFootnotesStartCollapsedAndHidden(t *testing.T) {
	t.Parallel()
	p := openFootnotesPage(t)
	var closed struct {
		Asked, Revisions bool
		Hidden           bool
		Pins             string
	}
	p.eval(`(() => {
  const card = document.querySelector(`+quote(amendmentCard)+`);
  const pins = card.querySelector('details.footnote.revisions [data-field=pins]');
  return {
    asked: card.querySelector('details.footnote.asked').open,
    revisions: card.querySelector('details.footnote.revisions').open,
    hidden: !pins.checkVisibility(),
    pins: pins.textContent,
  };
})()`, &closed)
	if closed.Asked || closed.Revisions || !closed.Hidden || !strings.Contains(closed.Pins, "amendment 1, packet revision 3") {
		t.Fatalf("the closed footnotes: %+v", closed)
	}
}

func TestBrowserDecisionFootnotesAreSetInMono(t *testing.T) {
	t.Parallel()
	p := openFootnotesPage(t)
	var mono bool
	p.eval(`(() => {
  const card = document.querySelector(`+quote(amendmentCard)+`);
  const pins = card.querySelector('details.footnote.revisions [data-field=pins]');
  return getComputedStyle(pins).fontFamily.includes('JetBrains Mono') && getComputedStyle(card.querySelector('details.footnote.revisions summary')).fontFamily.includes('JetBrains Mono');
})()`, &mono)
	if !mono {
		t.Fatal("the footnotes are not set in the mono face")
	}
}

func TestBrowserDecisionFootnoteRevisionsOpenInPlace(t *testing.T) {
	t.Parallel()
	p := openFootnotesPage(t)
	p.click(amendmentCard + "details.footnote.revisions summary")
	p.await("the revisions open in place", `(() => {
  const pins = document.querySelector(`+quote(amendmentCard+"[data-field=pins]")+`);
  return pins.checkVisibility() && !!pins.closest('[data-decision]');
})()`)
}

func TestBrowserDecisionFootnoteAskedShowsTheQuestion(t *testing.T) {
	t.Parallel()
	p := openFootnotesPage(t)
	p.click(amendmentCard + "details.footnote.asked summary")
	p.awaitText(amendmentCard+"[data-field=asked]", "May the budget rise?")
}

func TestBrowserDecisionFootnotesLookLikeTheDecidedByLabel(t *testing.T) {
	t.Parallel()
	p := openFootnotesPage(t)
	p.openActivity()
	p.awaitVisible("the decided by label", "#workstream-activity [data-field=decided-by]")
	var look struct {
		Asked, Revisions, DecidedBy [3]string
	}
	p.eval(`(() => {
  const style = (node) => { const s = getComputedStyle(node); return [s.fontFamily, s.fontSize, s.color]; };
  const card = document.querySelector(`+quote(amendmentCard)+`);
  return {
    asked: style(card.querySelector('details.footnote.asked summary')),
    revisions: style(card.querySelector('details.footnote.revisions summary')),
    decidedBy: style(document.querySelector('#workstream-activity [data-field=decided-by]')),
  };
})()`, &look)
	if look.Asked != look.DecidedBy || look.Revisions != look.DecidedBy || !strings.Contains(look.DecidedBy[0], "JetBrains Mono") {
		t.Fatalf("footnote looks differ: %+v", look)
	}
}

func TestBrowserDeliveryCardReadsAsAReleaseNote(t *testing.T) {
	t.Parallel()
	presentation := strings.Replace(deliveryPresentation(`{"state":"recorded","review":2,"review_revision":3,"commit":"abc123","text":"Owners see the new page."}`),
		`"summary":"Everything shown.",`, `"summary":"Everything shown.","findings":[{"severity":"advice","criteria":["spec#1"],"finding":"Retries are missing","action":"Add them"}],`, 1)
	p := newDeliveryBriefPage(t, presentation)
	submit := `[data-decision][data-kind="delivery"] button[type=submit]`
	p.await("the publish button ready", fmt.Sprintf(`document.querySelector(%s)?.disabled === false`, quote(submit)))
	var order string
	p.eval(`(() => {
  const card = document.querySelector('[data-decision][data-kind="delivery"]');
  const parts = {
    brief: '[data-field=brief]', findings: '[data-field=findings]', criteria: '[data-field=criteria]',
    preview: '[data-field=preview]', publish: 'button[type=submit]',
  };
  const nodes = Object.entries(parts).map(([name, selector]) => [name, card.querySelector(selector)]);
  const missing = nodes.filter(([, node]) => !node).map(([name]) => name);
  if (missing.length) return 'missing ' + missing.join(',');
  nodes.sort((a, b) => (a[1].compareDocumentPosition(b[1]) & Node.DOCUMENT_POSITION_FOLLOWING) ? -1 : 1);
  const last = card.lastElementChild;
  return nodes.map(([name]) => name).join(',') + (last.contains(card.querySelector('button[type=submit]')) ? ' publish last' : ' not last');
})()`, &order)
	if want := "brief,findings,criteria,preview,publish publish last"; order != want {
		t.Fatalf("the release note order: %q, want %q", order, want)
	}
}
