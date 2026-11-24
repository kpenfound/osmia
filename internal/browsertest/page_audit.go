package browsertest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/chromedp/chromedp"
)

// ScreenshotsEnv names the directory the Page writes screenshots to. The
// browser tests write nothing without it.
const ScreenshotsEnv = "OSMIA_SCREENSHOTS"

// Screenshot writes the Page as it is shown to name.png in the directory
// ScreenshotsEnv names, or does nothing without one. position, when given, is
// a script that scrolls the Page; it runs in the same step as the capture, so
// nothing the Page does in between moves the picture.
func (p *Page) Screenshot(name string, position ...string) {
	p.T.Helper()
	dir := os.Getenv(ScreenshotsEnv)
	if dir == "" {
		return
	}
	p.Await("the page at rest", `document.getAnimations().every(a => a.playState !== 'running' || a.effect.getTiming().iterations === Infinity)`)
	var png []byte
	actions := []chromedp.Action{}
	for _, script := range position {
		actions = append(actions, chromedp.Evaluate(script, nil))
	}
	p.Run(append(actions, chromedp.CaptureScreenshot(&png))...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.T.Fatalf("creating the screenshot directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0o600); err != nil {
		p.T.Fatalf("writing the screenshot %s: %v", name, err)
	}
}

const auditScript = `((phone) => {
  const findings = [];
  const visible = (e) => {
    const r = e.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) return false;
    const s = getComputedStyle(e);
    return s.visibility !== 'hidden' && s.display !== 'none' && !e.closest('[hidden], [inert]');
  };
  const scope = document.querySelector('dialog[open]') || document.body;
  const describe = (e) => e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + (e.className && typeof e.className === 'string' ? '.' + e.className.trim().split(/\s+/).join('.') : '') +
    ' "' + (e.getAttribute('aria-label') || e.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 40) + '"';

  // No separator character joins what the page shows.
  const joined = scope.innerText.split('\n').filter((line) => line.includes(' · '));
  for (const line of joined) findings.push('metadata joined with a separator: ' + line.slice(0, 80));
  for (const e of scope.querySelectorAll('[title], [aria-label]')) {
    const text = (e.getAttribute('title') || '') + (e.getAttribute('aria-label') || '');
    if (text.includes(' · ')) findings.push('a label joined with a separator: ' + describe(e));
  }

  // Nothing is set in capitals or capitalised by style.
  for (const e of scope.querySelectorAll('*')) {
    if (!visible(e)) continue;
    const transform = getComputedStyle(e).textTransform;
    if ((transform === 'uppercase' || transform === 'capitalize') && [...e.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim())) {
      findings.push('text-transform ' + transform + ': ' + describe(e));
    }
  }

  // Labels, headings and buttons read in sentence case: later words are not
  // capitalised unless they name a product or are an acronym.
  const names = new Set(['Osmia', 'Dagger', 'GitHub', 'Jev', 'Git', 'Docker', 'Tailscale', 'Tailnet', 'Chromium', 'Cloud', 'I', 'OK']);
  const acronyms = new Set(['ID', 'IDs', 'URL', 'URLs', 'CLI', 'API', 'PR', 'HTTP', 'HTTPS', 'TLS', 'SHA', 'UI', 'AI', 'JSON', 'TOML', 'LLM', 'CPU', 'OK', 'AGENTS']);
  const chrome = 'button, h1, h2, h3, h4, summary, label, legend, th, option, [role=tab], [role=menuitem]';
  const data = '#workstream-list, #archived-list, .needs-list, .project-card, .pane-workstream, .pane-workgroup, .row, .feed, .activity-log, .markdown, .decision .question, [data-field=brief], select, .member-card, #workstream-head [data-field=goal], .head-title';
  for (const e of scope.querySelectorAll(chrome)) {
    if (!visible(e) || e.closest(data)) continue;
    const text = (e.firstElementChild && e.matches('label') ? [...e.childNodes].filter((n) => n.nodeType === 3).map((n) => n.textContent).join(' ') : e.textContent).trim().replace(/\s+/g, ' ');
    if (!text) continue;
    const words = text.split(' ').map((w) => w.replace(/^[("']+|[.,:;!?)"'…]+$/g, ''));
    const loud = words.slice(1).filter((w) => /^[A-Z]/.test(w) && /[a-z]/.test(w) && !names.has(w) && !/[\d._/]/.test(w));
    if (loud.length > 0) findings.push('not sentence case: ' + describe(e));
    const caps = words.filter((w) => /^[A-Z]{3,}$/.test(w) && !acronyms.has(w) && !names.has(w));
    if (caps.length > 0) findings.push('all caps ' + caps.join(',') + ': ' + describe(e));
  }

  // No rule transitions every property.
  for (const sheet of document.styleSheets) {
    let rules = [];
    try { rules = [...sheet.cssRules]; } catch (_) { continue; }
    const walk = (list) => {
      for (const rule of list) {
        if (rule.cssRules && !rule.style) { walk(rule.cssRules); continue; }
        if (rule.style && (rule.style.getPropertyValue('transition-property') === 'all' || /(^|\s|,)all(\s|,|$)/.test(rule.style.getPropertyValue('transition')))) {
          findings.push('transition: all in ' + rule.selectorText);
        }
      }
    };
    walk(rules);
  }
  if (!getComputedStyle(document.documentElement).colorScheme.includes('dark')) findings.push('the page does not declare color-scheme: dark');

  // The page never scrolls sideways.
  if (document.documentElement.scrollWidth > document.documentElement.clientWidth) {
    findings.push('the page scrolls sideways: ' + document.documentElement.scrollWidth + 'px in ' + document.documentElement.clientWidth + 'px');
  }

  // On a phone every control can be pressed on a 44px square: the area that
  // answers a press, pseudo-element included, found by probing outward.
  if (phone) {
    const controls = scope.querySelectorAll('button, a[href], input:not([type=hidden]):not([type=file]), select, textarea, summary, [role=tab]');
    for (const raw of controls) {
      if (!visible(raw) || raw.closest('.copy-scratch')) continue;
      if (raw.matches('a') && getComputedStyle(raw).display === 'inline') continue;
      const e = raw.matches('input[type=checkbox], input[type=radio]') ? raw.closest('label') || raw : raw;
      e.scrollIntoView({ block: 'center', inline: 'center' });
      const r = e.getBoundingClientRect();
      const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
      const owns = (x, y) => {
        if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) return false;
        const n = document.elementFromPoint(x, y);
        return !!n && (n === e || e.contains(n));
      };
      if (!owns(cx, cy)) continue;
      const reach = (dx, dy) => { let d = 0; while (d < 60 && owns(cx + dx * (d + 1), cy + dy * (d + 1))) d++; return d; };
      const w = reach(1, 0) + reach(-1, 0) + 1, h = reach(0, 1) + reach(0, -1) + 1;
      if (w < 43.5 || h < 43.5) findings.push('a control under 44px (' + w + 'x' + h + '): ' + describe(e));
    }
  }
  return [...new Set(findings)];
})`

// Audit fails the test for every way the shown page breaks the design rules
// that can be read from it: separators in metadata, capitals, Title Case in
// a label, a transition of every property, a sideways-scrolling page and, on
// a phone, a control smaller than 44px square. where names the step.
func (p *Page) Audit(where string, phone bool) {
	p.T.Helper()
	var findings []string
	p.Eval(fmt.Sprintf("%s(%t)", auditScript, phone), &findings)
	for _, f := range findings {
		p.T.Errorf("%s: %s", where, f)
	}
}

// Stickiness is how much of the viewport the sticky header and message field
// cover, and how much is left for the conversation between them.
type Stickiness struct {
	Viewport, Top, Bottom, Between float64
	Sideways                       bool
}

// MeasureSticky reads how much of the viewport the workstream's sticky header
// and the message field cover.
func (p *Page) MeasureSticky() Stickiness {
	p.T.Helper()
	var s Stickiness
	p.Eval(`(() => {
  const covered = (selector) => {
    const e = document.querySelector(selector);
    if (!e || e.closest('[hidden]') || getComputedStyle(e).position !== 'sticky') return 0;
    const r = e.getBoundingClientRect();
    return Math.max(0, Math.min(r.bottom, innerHeight) - Math.max(r.top, 0));
  };
  const top = covered('.view:not([hidden]) .workstream-top');
  const bottom = covered('.view:not([hidden]) .composer');
  return { viewport: innerHeight, top, bottom, between: innerHeight - top - bottom, sideways: document.documentElement.scrollWidth > document.documentElement.clientWidth };
})()`, &s)
	return s
}
