'use strict';

(() => {
  const api = '/v1';

  // The views each event kind names. "feed" is the feed of the event's
  // workstream and "feeds" that of every workstream the page shows. Kinds the
  // page does not show are ignored; every stream starts with a resync.
  const reads = {
    resync: ['status', 'runtime', 'config', 'inbox', 'charter', 'feeds', 'activity'],
    workstream: ['status', 'feed'],
    conversation: ['feed'],
    inbox: ['inbox', 'charter'],
    runtime: ['runtime', 'status'],
    config: ['config'],
    spend: ['status'],
    capacity: ['status'],
    activity: ['activity'],
  };

  // activityStallMs mirrors the service's ActivityStallThreshold: an
  // activity entry whose last progress is at least this old is shown as
  // blocked, the same as a failed one, even without a fresh read. It does
  // not apply to a turn or a check (activityStallTrackedKinds mirrors the
  // service's stallTracked): those report only start and finish and
  // routinely run for minutes, so elapsed time alone does not mean blocked.
  // Reconciled Git and workspace operations (seal, build, land, rebase,
  // drift, publish) are the same: the service reports them with only a
  // start and a finish and never calls Progress on them today, so the
  // server-side rule never flags one as blocked from elapsed time alone
  // either (activity.go, ActivityEntry.reported) and "git" is left out here
  // to match. archive_cleanup and recovery do report progress through their
  // phases, so a stall in either still shows as blocked.
  const activityStallMs = 30000;
  const activityStallTrackedKinds = new Set(['archive_cleanup', 'recovery']);

  // An edit of a configuration file on disk is not announced, so the page
  // reads /config again this often while it is visible and live.
  const configInterval = 10000;

  // How often the page checks whether a dismissed feed action item has
  // passed its DISMISS_TIMEOUT_MS comeback window. This runs independently
  // of any other activity, so a dismissal still clears on time even if
  // nothing else prompts a render.
  const dismissCheckInterval = 1000;

  // settingsSections are the sections of the Settings view; opening one
  // shows the view on it.
  const settingsSections = ['projects', 'priority', 'profiles', 'config', 'engine'];
  const unitOrder = ['planned', 'ready', 'implementing', 'waiting', 'checking', 'reviewing', 'approved', 'contested', 'merged'];

  const pauseSources = {
    owner: 'the owner',
    'daily-budget': 'the daily budget',
    'provider-usage-limit': 'a provider usage limit',
    'loop-guard': 'the loop guard',
  };

  const waitReasons = {
    capacity: 'every slot is taken',
    priority: 'higher-priority work starts first',
    'workstream-cap': 'the workstream holds its share',
  };

  const profileSources = {
    configuration: 'configured',
    owner_override: 'owner override',
    provider_fallback: 'provider fallback',
    provider_pause: 'paused by a provider limit',
  };

  // views holds each view by its path after /v1: status, runtime, config,
  // inbox, feed/<workstream-id>, and packet/<workstream-id> and
  // delivery/<workstream-id> for the ratifications and deliveries the inbox
  // lists.
  const views = { status: null, runtime: null, config: null, inbox: null };
  const failures = {};
  const dirty = new Set();
  let reading = false;
  let retries = 0;
  let retryTimer = null;

  // stored reads one of this browser's own preferences, and store writes
  // one. A browser that refuses storage keeps them for the page's lifetime.
  function stored(key, fallback) {
    try {
      const raw = localStorage.getItem('osmia.' + key);
      return raw === null ? fallback : JSON.parse(raw);
    } catch {
      return fallback;
    }
  }

  function store(key, value) {
    try {
      localStorage.setItem('osmia.' + key, JSON.stringify(value));
    } catch {
      // The preference lasts until the page closes.
    }
  }

  // workstreamTabs are the tabs of a workstream, the first being the default.
  const workstreamTabs = ['conversation', 'activity', 'documents', 'trace'];

  // parseRoute reads the page's location hash. A hash names what the main
  // area shows: #workstream=<id> or #workgroup=<id>, followed for a
  // workstream by the tab, the Activity unit filter and the opened
  // Activity entries as &tab=, &unit=, &open= and &entry=.
  function parseRoute(hash) {
    const [head, ...rest] = hash.replace(/^#/, '').split('&');
    const route = { kind: '', id: '', tab: 'conversation', unit: '', open: [], entry: '' };
    const at = head.indexOf('=');
    const kind = head.slice(0, at);
    if (at < 0 || (kind !== 'workstream' && kind !== 'workgroup')) return route;
    const decode = (text) => { try { return decodeURIComponent(text); } catch { return text; } };
    route.kind = kind;
    route.id = decode(head.slice(at + 1));
    for (const part of rest) {
      const eq = part.indexOf('=');
      const name = part.slice(0, eq);
      const value = part.slice(eq + 1);
      if (name === 'tab' && workstreamTabs.includes(value)) route.tab = value;
      if (name === 'unit') route.unit = decode(value);
      if (name === 'open') route.open = value.split(',').filter(Boolean).map(decode);
      if (name === 'entry') route.entry = decode(value);
    }
    return route;
  }

  const startRoute = parseRoute(location.hash);

  // ui is what the owner is looking at: the selected workstream, the view
  // the main area shows and the workstream's tab. unit, open and focus are
  // the Activity tab's unit filter, its opened entries and the entry a
  // milestone or link pointed at. seen holds the activity the owner last saw
  // on each workstream. initial holds the workstreams the first status read
  // listed, whose current activity counts as seen the first time the page
  // shows them. navigated says the owner has opened a view since the page
  // loaded, so the first status read no longer picks the view from the link.
  const ui = {
    selected: startRoute.kind === 'workstream' ? startRoute.id : stored('selected', null),
    section: 'projects',
    view: startRoute.kind === 'workgroup' ? 'workgroup' : startRoute.kind === 'workstream' ? 'workstream' : 'project-pane',
    aboutMember: null,
    workgroup: startRoute.kind === 'workgroup' ? startRoute.id : null,
    tab: startRoute.kind === 'workstream' ? startRoute.tab : 'conversation',
    unit: startRoute.kind === 'workstream' && startRoute.tab === 'activity' ? startRoute.unit : '',
    open: new Set(startRoute.kind === 'workstream' && startRoute.tab === 'activity' ? startRoute.open : []),
    focus: startRoute.kind === 'workstream' && startRoute.tab === 'activity' ? startRoute.entry : '',
    scrollFocus: startRoute.kind === 'workstream' && startRoute.tab === 'activity' && startRoute.entry !== '',
    seen: stored('seen', {}),
    initial: null,
    navigated: false,
    toEnd: true,
  };

  // routeHash is the location hash for what the owner is looking at.
  function routeHash() {
    const enc = encodeURIComponent;
    if (ui.view === 'workgroup' && ui.workgroup) return '#workgroup=' + enc(ui.workgroup);
    if (!ui.selected || !['workstream', 'session', 'design'].includes(ui.view)) return '';
    const parts = ['workstream=' + enc(ui.view === 'workstream' ? shown()?.workstream || ui.selected : ui.selected)];
    if (ui.view === 'workstream') {
      if (ui.tab !== 'conversation') parts.push('tab=' + ui.tab);
      if (ui.tab === 'activity') {
        if (ui.unit) parts.push('unit=' + enc(ui.unit));
        if (ui.open.size > 0) parts.push('open=' + [...ui.open].map(enc).join(','));
        if (ui.focus) parts.push('entry=' + enc(ui.focus));
      }
    }
    return '#' + parts.join('&');
  }

  // syncRoute writes the view into the location. A change the Back button
  // should undo, such as a new tab or filter, pushes a history entry; the
  // rest replaces the current one.
  function syncRoute(push = false) {
    const hash = routeHash();
    if (hash === '' || hash === location.hash) return;
    if (push) {
      history.pushState(null, '', hash);
    } else {
      history.replaceState(null, '', hash);
    }
  }

  // el builds an element; strings among the children become text nodes.
  // edgeLine is the line the workstream graph shows for an edge. The shared
  // graph keeps naming an edge its dependent dropped, so the line says it was
  // dropped, and never that the upstream merged.
  function edgeLine(group, e, text) {
    const dropped = !!group.dropped?.some((d) => d.from === e.from && d.to === e.to && d.type === e.type);
    const attrs = dropped ? { 'data-edge': e.type, 'data-dropped': 'true' } : { 'data-edge': e.type };
    return el('p', attrs, dropped ? text + ', dropped (not merged)' : text);
  }

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(attrs || {})) {
      if (key === 'class') {
        node.className = value;
      } else {
        node.setAttribute(key, value);
      }
    }
    for (const child of children) {
      if (child !== null && child !== undefined && child !== false) {
        node.append(child);
      }
    }
    return node;
  }

  function byId(id) {
    return document.getElementById(id);
  }

  // metaRow lays separate facts out in a row with a gap between them. A
  // string part becomes its own span; a node part is kept as it is, so it
  // can carry its own data attributes. The text between parts is a space,
  // never a separator character.
  function metaRow(...parts) {
    const out = [];
    for (const part of parts.filter((p) => p !== null && p !== undefined && p !== false && p !== '')) {
      if (out.length > 0) out.push(' ');
      out.push(typeof part === 'string' ? el('span', {}, part) : part);
    }
    return el('span', { class: 'meta-row' }, ...out);
  }

  // Markdown is built only from text nodes and known elements. Agent HTML
  // never passes through an HTML parser, and links require an explicit scheme.
  function markdownClosingMark(text, mark, start) {
    let end = text.indexOf(mark, start);
    while (end !== -1 && (text[end - 1] === mark[0] || text[end + mark.length] === mark[0])) {
      end = text.indexOf(mark, end + mark.length);
    }
    return end;
  }

  function markdownInline(text, depth = 0) {
    const out = document.createDocumentFragment();
    if (depth > 24) {
      out.append(text);
      return out;
    }
    let literal = '';
    const flush = () => { out.append(literal); literal = ''; };
    for (let i = 0; i < text.length;) {
      if (text[i] === '`') {
        const mark = /^`+/.exec(text.slice(i))[0];
        const end = markdownClosingMark(text, mark, i + mark.length);
        if (end > i + mark.length) {
          flush();
          out.append(el('code', {}, text.slice(i + mark.length, end)));
          i = end + mark.length;
          continue;
        }
        literal += mark;
        i += mark.length;
        continue;
      }
      if (text[i] === '[' && text[i - 1] !== '!') {
        const labelEnd = text.indexOf('](', i + 1);
        if (labelEnd !== -1) {
          let end = labelEnd + 2;
          let nesting = 1;
          for (; end < text.length; end++) {
            if (text[end] === '(') nesting++;
            if (text[end] === ')' && --nesting === 0) break;
          }
          if (nesting === 0) {
            const target = text.slice(labelEnd + 2, end);
            flush();
            if (/^(https?:|mailto:)[^\s<>]+$/i.test(target)) {
              out.append(el('a', { href: target }, markdownInline(text.slice(i + 1, labelEnd), depth + 1)));
            } else {
              out.append(text.slice(i, end + 1));
            }
            i = end + 1;
            continue;
          }
          // An unfinished target stays literal, including any inline markers.
          literal += text.slice(i);
          break;
        }
      }
      if (text[i] === '*' || text[i] === '_') {
        const char = text[i];
        const mark = new RegExp('^' + (char === '*' ? '\\*' : '_') + '+').exec(text.slice(i))[0];
        const end = markdownClosingMark(text, mark, i + mark.length);
        // Underscores within words and whitespace-only spans stay literal.
        if (mark.length <= 3 && end > i + mark.length && !/\s/.test(text[i + mark.length]) &&
            !/\s/.test(text[end - 1]) && !(char === '_' && /\w/.test(text[i - 1] || ''))) {
          flush();
          const body = markdownInline(text.slice(i + mark.length, end), depth + 1);
          out.append(mark.length === 3 ? el('strong', {}, el('em', {}, body)) :
            el(mark.length === 2 ? 'strong' : 'em', {}, body));
          i = end + mark.length;
          continue;
        }
        literal += mark;
        i += mark.length;
        continue;
      }
      literal += text[i++];
    }
    flush();
    return out;
  }

  function markdownCells(line) {
    let value = line.trim();
    if (!value.includes('|')) return null;
    if (value.startsWith('|')) value = value.slice(1);
    if (value.endsWith('|') && !value.endsWith('\\|')) value = value.slice(0, -1);
    // Pipes in code spans or escaped pipes belong to the cell's text.
    const cells = [];
    let cell = '';
    let ticks = '';
    for (let i = 0; i < value.length;) {
      if (value[i] === '\\' && value[i + 1] === '|') {
        cell += '|';
        i += 2;
      } else if (value[i] === '`') {
        const mark = /^`+/.exec(value.slice(i))[0];
        if (!ticks) ticks = mark;
        else if (ticks === mark) ticks = '';
        cell += mark;
        i += mark.length;
      } else if (value[i] === '|' && !ticks) {
        cells.push(cell.trim());
        cell = '';
        i++;
      } else {
        cell += value[i++];
      }
    }
    cells.push(cell.trim());
    return cells;
  }

  function renderMarkdown(text, depth = 0) {
    const out = el('div', { class: 'markdown' });
    const lines = text.split('\n');
    const fence = (line) => /^ {0,3}(`{3,}|~{3,})([^`]*)$/.exec(line);
    const list = (line) => /^( *)([-+*]|\d+[.)]) +(.*)$/.exec(line);
    const heading = (line) => /^ {0,3}(#{1,6}) +(.+)$/.exec(line);
    const quote = (line) => /^ {0,3}> ?(.*)$/.exec(line);
    const table = (i) => {
      const cells = markdownCells(lines[i] || '');
      const rules = markdownCells(lines[i + 1] || '');
      return cells && rules && cells.length === rules.length && rules.every((s) => /^:?-{3,}:?$/.test(s)) ? { cells, rules } : null;
    };
    if (depth > 24) {
      out.append(el('p', {}, text));
      return out;
    }
    for (let i = 0; i < lines.length;) {
      if (!lines[i].trim()) {
        out.append(lines[i]);
        if (i < lines.length - 1) out.append('\n');
        i++;
        continue;
      }
      const f = fence(lines[i]);
      const h = heading(lines[i]);
      const q = quote(lines[i]);
      const item = list(lines[i]);
      const t = table(i);
      if (f) {
        let end = i + 1;
        const closing = new RegExp('^ {0,3}' + f[1][0] + '{' + f[1].length + ',}\\s*$');
        while (end < lines.length && !closing.test(lines[end])) end++;
        if (end === lines.length) {
          out.append(el('p', {}, lines.slice(i).join('\n')));
          break;
        }
        out.append(el('pre', {}, el('code', {}, lines.slice(i + 1, end).join('\n'))));
        i = end + 1;
      } else if (h) {
        out.append(el('h' + h[1].length, {}, markdownInline(h[2])));
        i++;
      } else if (q) {
        const body = [];
        while (i < lines.length && quote(lines[i])) body.push(quote(lines[i++])[1]);
        out.append(el('blockquote', {}, renderMarkdown(body.join('\n'), depth + 1)));
      } else if (item) {
        const indent = item[1].length;
        const ordered = /^\d/.test(item[2]);
        const node = el(ordered ? 'ol' : 'ul', ordered ? { start: parseInt(item[2], 10) } : {});
        while (i < lines.length) {
          const next = list(lines[i]);
          if (!next || next[1].length !== indent || /^\d/.test(next[2]) !== ordered) break;
          const body = [next[3]];
          const contentIndent = next[1].length + next[2].length + 1;
          i++;
          while (i < lines.length) {
            if (!lines[i].trim()) {
              let after = i + 1;
              while (after < lines.length && !lines[after].trim()) after++;
              if (after >= lines.length || lines[after].search(/\S/) <= indent) break;
              body.push(lines[i++]);
            } else if (lines[i].search(/\S/) > indent) {
              body.push(lines[i].slice(Math.min(contentIndent, lines[i].search(/\S/))));
              i++;
            } else break;
          }
          node.append(el('li', {}, renderMarkdown(body.join('\n'), depth + 1)));
        }
        out.append(node);
      } else if (t) {
        let end = i + 2;
        const rows = [];
        let malformed = false;
        while (end < lines.length && lines[end].includes('|') && lines[end].trim()) {
          const cells = markdownCells(lines[end++]);
          if (cells.length !== t.cells.length) malformed = true;
          rows.push(cells);
        }
        if (malformed) {
          out.append(el('p', {}, lines.slice(i, end).join('\n')));
        } else {
          const row = (cells, tag) => el('tr', {}, ...cells.map((cell, column) => {
            const rule = t.rules[column];
            const align = rule.endsWith(':') ? (rule.startsWith(':') ? 'center' : 'right') : 'left';
            return el(tag, { class: 'align-' + align }, markdownInline(cell));
          }));
          out.append(el('div', { class: 'markdown-table', tabindex: '0', role: 'region', 'aria-label': 'Table' },
            el('table', {}, el('thead', {}, row(t.cells, 'th')), el('tbody', {}, ...rows.map((cells) => row(cells, 'td'))))));
        }
        i = end;
      } else {
        const body = [lines[i++]];
        while (i < lines.length && lines[i].trim() && !fence(lines[i]) && !heading(lines[i]) &&
            !quote(lines[i]) && !list(lines[i]) && !table(i)) body.push(lines[i++]);
        out.append(el('p', {}, markdownInline(body.join('\n'))));
      }
      // Keep source line separators in textContent as well as the block
      // layout, including blank lines in otherwise plain text.
      if (i < lines.length) out.append('\n');
    }
    return out;
  }

  // request calls the API and returns its answer, or throws an error with
  // the API's message when it refuses. The thrown error's code is the
  // API's error code, 'unavailable' for a command the backend cannot admit
  // right now (a blocking operation is named in the message), or undefined
  // when the API gave no structured error, so act can tell that outcome
  // apart from an ordinary rejection.
  async function request(method, path, body) {
    const init = { method, cache: 'no-store', headers: { Accept: 'application/json' } };
    if (method !== 'GET') {
      init.headers['Content-Type'] = 'application/json';
    }
    if (body !== undefined) {
      init.body = JSON.stringify(body);
    }
    let response;
    try {
      response = await fetch(api + path, init);
    } catch {
      throw new Error('Osmia cannot be reached; try again once the page is live.');
    }
    const out = await response.json().catch(() => null);
    if (!response.ok) {
      const refusal = out && out.error && out.error.message ? out.error.message : response.status + ' ' + response.statusText;
      const err = new Error(refusal);
      if (out && out.error && out.error.code) {
        err.code = out.error.code;
      }
      if (out && out.error && out.error.field) {
        err.field = out.error.field;
      }
      throw err;
    }
    return out;
  }

  // listedFeeds names the feeds the page reads: those of the workstreams in
  // the list of work. Archived workstreams have no readable history. A
  // workgroup whose members are all archived keeps its shared conversation, which
  // is read only while the owner has that workgroup open.
  function listedFeeds() {
    return [...streams().filter(w => !w.archived).map(w => 'feed/' + w.workstream),
      ...workgroups().filter(g => groupLive(g.id) || (ui.view === 'workgroup' && ui.workgroup === g.id)).map(g => groupFeed(g.id))];
  }

  // details holds, for each packet and delivery view, the entry it was read
  // for: its revision and the identity its answer pins.
  const details = new Map();

  function detailOf(entry) {
    switch (entry.kind) {
      case 'ratification':
        return 'packet/' + entry.workstream;
      case 'delivery':
        return 'delivery/' + entry.workstream;
      default:
        return null;
    }
  }

  // listedDetails returns the packet and delivery views of the entries the
  // inbox lists, each with the entry it is read for.
  function listedDetails() {
    const out = new Map();
    for (const entry of views.inbox ? views.inbox.entries : []) {
      const name = detailOf(entry);
      if (name) {
        out.set(name, JSON.stringify([entry.revision, entry.answer.body]));
      }
    }
    return out;
  }

  // refresh reads every view marked since the last read, once each, and
  // renders; views marked while it reads are read again after. A
  // workstream the status lists gets its feed read, and one it no longer
  // lists loses it. A ratification or delivery the inbox lists gets
  // its packet or delivery read, again whenever the entry's revision or
  // identity changes, and again with the next read of the inbox after a read
  // of it failed.
  async function refresh() {
    if (reading) {
      return;
    }
    reading = true;
    try {
      while (dirty.size > 0) {
        const names = [...dirty];
        const inboxRead = names.includes('inbox');
        dirty.clear();
        await Promise.all(names.map(async (name) => {
          try {
            const pin = charterRead;
            const configPin = configRead;
            const out = await request('GET', '/' + name);
            const previous = views[name];
            views[name] = out;
            if (name === 'status') {
              for (const w of out.workstreams) {
                const old = previous?.workstreams.find(x => x.workstream === w.workstream);
                if (isMultiMember(w) && old && !isMultiMember(old)) {
                  delete views['feed/' + w.workstream];
                  delete failures['feed/' + w.workstream];
                  dirty.add('feed/' + w.workstream);
                  // The member's unread baseline now covers only its own work.
                  delete ui.seen[w.workstream];
                  ui.initial?.add(w.workstream);
                }
              }
            }
            if (name.startsWith('projects/charter/')) {
              const project = name.slice('projects/charter/'.length);
              if (byId('project-edit').elements.project.value === project && charterRead === pin) {
                receiveCharter(project, out);
              }
            }
            if (name.startsWith('projects/config/')) {
              const project = name.slice('projects/config/'.length);
              if (byId('project-edit').elements.project.value === project && configRead === configPin) {
                receiveConfig(project, out);
              }
            }
            delete failures[name];
          } catch (err) {
            if (!isManagementView(name) || name.endsWith('/' + byId('project-edit').elements.project.value)) {
              failures[name] = err.message;
            }
          }
        }));
        if (views.status) {
          if (!streams().some(w => w.workstream === ui.selected)) {
            const member = streams().find(w => w.workgroup?.conversation?.workstream === ui.selected);
            if (member) {
              carryOutcomes(ui.selected, member.workstream);
              ui.selected = member.workstream;
              store('selected', ui.selected);
              if (ui.view === 'design' || ui.view === 'workstream') {
                ui.view = isMultiMember(member) ? 'workgroup' : 'workstream';
                ui.workgroup = member.workgroup?.id || null;
                ui.tab = 'conversation';
                ui.unit = '';
                ui.open.clear();
                ui.focus = '';
                syncRoute();
              }
            }
          }
          if (ui.initial === null && !ui.navigated && startRoute.kind === 'workstream') {
            const w = streams().find(w => w.workstream === ui.selected);
            if (w) {
              ui.view = w.kind === 'session' ? 'session' : w.kind === 'design' ? 'design' : 'workstream';
              if (w.kind === 'design') {
                dirty.add('design/' + w.workstream);
                dirty.add('design/' + w.workstream + '/conversation');
              }
              if (w.kind === 'session') {
                dirty.add('sessions/' + w.workstream + '/changes');
              }
            }
          }
          for (const w of streams()) {
            if (w.kind !== 'design' && w.state && designIds.has(w.workstream)) {
              designIds.delete(w.workstream);
              startedDesignIds.add(w.workstream);
              if (ui.selected === w.workstream && ui.view === 'design') {
                ui.view = 'workstream';
                ui.tab = 'conversation';
              }
            }
          }
          const listed = new Set(listedFeeds());
          for (const name of [...Object.keys(views), ...Object.keys(failures)]) {
            if ((name.startsWith('feed/') || /^workgroups\/.+\/feed$/.test(name)) && !listed.has(name)) {
              delete views[name];
              delete failures[name];
            }
          }
          for (const name of listed) {
            if (!(name in views) && !(name in failures)) {
              dirty.add(name);
            }
          }
        }
        if (views.inbox) {
          const listed = listedDetails();
          for (const name of [...details.keys()]) {
            if (!listed.has(name)) {
              details.delete(name);
              delete views[name];
              delete failures[name];
            }
          }
          for (const [name, entry] of listed) {
            if (details.get(name) !== entry || (inboxRead && name in failures)) {
              details.set(name, entry);
              dirty.add(name);
            }
          }
        }
        render();
        if (names.some(name => ['status', 'config', 'charter'].includes(name))) {
          const project = byId('project-edit').elements.project.value;
          if (project) {
            dirty.add('projects/charter/' + project);
            dirty.add('projects/config/' + project);
          }
        }
      }
    } finally {
      reading = false;
    }
  }

  function mark(names) {
    for (const name of names) {
      dirty.add(name);
    }
    refresh();
  }

  // markFromEvent gathers the views events name for eventReadDelayMs before
  // marking them, so a burst of commits reads each view once rather than
  // once per event. A resync and the owner's own actions are read at once.
  const eventReadDelayMs = 300;
  const eventViews = new Set();
  let eventTimer = null;
  function markFromEvent(names) {
    for (const name of names) {
      eventViews.add(name);
    }
    if (eventTimer !== null) {
      return;
    }
    eventTimer = setTimeout(() => {
      eventTimer = null;
      const pending = [...eventViews];
      eventViews.clear();
      mark(pending);
    }, eventReadDelayMs);
  }

  // expand turns an event's view names into view paths.
  function expand(name, event) {
    if (name === 'feeds') {
      return [...listedFeeds(), ...streams().filter((w) => isDesignId(w.workstream) && !w.archived)
        .flatMap((w) => ['design/' + w.workstream, 'design/' + w.workstream + '/conversation'])];
    }
    if (name === 'feed') {
      let data = {};
      try {
        data = JSON.parse(event.data);
      } catch {
        return [];
      }
      if (!data.workstream) {
        return [];
      }
      return isDesignId(data.workstream)
        ? ['feed/' + data.workstream, 'design/' + data.workstream, 'design/' + data.workstream + '/conversation']
        : ['feed/' + data.workstream, ...workgroups().filter(g => g.members.includes(data.workstream) || g.conversation?.workstream === data.workstream).map(g => groupFeed(g.id))];
    }
    return [name];
  }

  function setConnection(state) {
    document.body.dataset.connection = state;
    const label = { connecting: 'Connecting…', live: 'Live', lost: 'Connection lost, reconnecting…' }[state];
    byId('connection').title = label;
    byId('connection').querySelector('.label').textContent = label;
  }

  // connect opens the event stream. A lost stream is closed and opened again
  // after a growing delay; the new stream's resync reads everything again.
  function connect() {
    retryTimer = null;
    const stream = new EventSource(api + '/events');
    stream.onopen = () => {
      retries = 0;
      setConnection('live');
    };
    for (const [kind, names] of Object.entries(reads)) {
      stream.addEventListener(kind, (event) => {
        const expanded = names.flatMap((name) => expand(name, event));
        if (kind === 'resync') {
          mark(expanded);
        } else {
          markFromEvent(expanded);
        }
      });
    }
    stream.onerror = () => {
      stream.close();
      setConnection('lost');
      const delay = Math.min(1000 * 2 ** retries, 10000);
      retries++;
      retryTimer = setTimeout(connect, delay);
    };
  }

  function reconnectNow() {
    if (retryTimer !== null) {
      clearTimeout(retryTimer);
      connect();
    }
  }

  // Shared result elements retain a separate outcome for each instance.
  // An action captures its entry before awaiting the API, so a later
  // selection can only paint the entry belonging to the displayed instance.
  const outcomeScopes = new Map();
  const buttonScopes = new Map();

  function scopeOutcome(result, instance, buttons) {
    const scope = { result, instance, entries: new Map() };
    outcomeScopes.set(result, scope);
    for (const button of buttons) scopeButton(button, instance);
    return scope;
  }

  function scopeButton(button, instance) {
    const scope = { instance, pending: new Map() };
    buttonScopes.set(button, scope);
    return scope;
  }

  function captureOutcome(result) {
    const scope = outcomeScopes.get(result);
    return scope ? { scope, key: scope.instance() } : result;
  }

  // carryOutcomes moves the outcomes shown for one selection to the
  // selection that replaces it, such as a started design session's start
  // result when its conversation continues as a workgroup member.
  function carryOutcomes(from, to) {
    for (const scope of outcomeScopes.values()) {
      if (scope.entries.has(from) && !scope.entries.has(to)) {
        scope.entries.set(to, scope.entries.get(from));
      }
    }
  }

  function paintOutcome(scope) {
    const entry = scope.entries.get(scope.instance());
    scope.result.dataset.outcome = entry ? entry.outcome : '';
    scope.result.textContent = entry ? entry.text : '';
  }

  function paintBusy(button, scope) {
    if (scope.pending.has(scope.instance())) button.dataset.busy = 'true';
    else delete button.dataset.busy;
    button.disabled = button.dataset.blocked === 'true' || button.dataset.busy === 'true';
  }

  function renderOutcomes() {
    for (const scope of outcomeScopes.values()) paintOutcome(scope);
    for (const [button, scope] of buttonScopes) paintBusy(button, scope);
  }

  // show writes an action's outcome: pending, ok, unavailable or error.
  function show(result, outcome, text) {
    const target = result.scope ? result : captureOutcome(result);
    if (target.scope) {
      target.scope.entries.set(target.key, { outcome, text });
      paintOutcome(target.scope);
    } else {
      result.dataset.outcome = outcome;
      result.textContent = text;
    }
  }

  let engineOpening = 0;

  function setupOutcomeScopes() {
    for (const action of ['restart', 'prune']) {
      scopeOutcome(byId('engine-' + action + '-result'), () => engineOpening, [byId('engine-' + action)]);
    }
    const selected = () => ui.selected;
    for (const [result, buttons] of [
      ['#design-result', '#design-form [type=submit]'],
      ['#design-start-result', '#design-start'],
      ['#design-destroy-result', '#design-destroy'],
      ['#session-result', '#session-form [type=submit]'],
      ['#session-control-result', '#session-stop, #session-check, #session-push, #session-end, #session-destroy'],
      ['#session-commit-form .result', '#session-commit-form [type=submit]'],
    ]) {
      scopeOutcome(document.querySelector(result), selected, document.querySelectorAll(buttons));
    }
    // Archived or missing selections display a different live workstream.
    const displayedWorkstream = () => ui.view === 'workgroup' ? 'workgroup:' + ui.workgroup : shown()?.workstream || null;
    for (const [result, buttons] of [
      ['#inbox-result', '[data-workstream-action]'],
      ...workstreamForms.map(id => ['#' + id + ' .result', '#' + id + ' button']),
    ]) {
      scopeOutcome(document.querySelector(result), displayedWorkstream, document.querySelectorAll(buttons));
    }
    for (const id of ['handin-form', 'design-add-form', 'session-add-form']) {
      const form = byId(id);
      let opening = 0;
      const scope = scopeOutcome(form.querySelector('.result'), () => opening, form.querySelectorAll('button'));
      form.elements.project.addEventListener('change', () => {
        opening++;
        scope.entries.clear();
        renderOutcomes();
      });
    }
  }

  // act runs one owner action with its button disabled and shows what the
  // API answered in result: done's text once the command is accepted and
  // durably recorded ('ok'), the API's message naming the blocking
  // operation when the command is temporarily unavailable
  // ('unavailable'), or the refusal's reason when it is rejected
  // ('error'); done may instead return {outcome, text} to name another
  // outcome of an accepted command; refused runs after either kind of refusal, when given. Once
  // the action ends, the displayed instance's pending actions and render's
  // blocked flag determine whether the button is enabled.
  async function act(result, button, call, done, refused) {
    result = captureOutcome(result);
    const scope = buttonScopes.get(button) || (result.scope && scopeButton(button, result.scope.instance));
    const key = scope ? scope.instance() : null;
    if (scope) scope.pending.set(key, (scope.pending.get(key) || 0) + 1);
    button.dataset.busy = 'true';
    button.disabled = true;
    show(result, 'pending', 'Sending…');
    try {
      const text = done(await call());
      if (text !== null && typeof text === 'object' && 'feed' in text) acceptOutcome(result, text.feed);
      else if (text !== null && typeof text === 'object') show(result, text.outcome, text.text);
      else show(result, 'ok', text);
    } catch (err) {
      show(result, err.code === 'unavailable' ? 'unavailable' : 'error', err.message);
      if (refused) {
        refused(err);
      }
    } finally {
      if (scope) {
        const remaining = scope.pending.get(key) - 1;
        if (remaining) scope.pending.set(key, remaining);
        else scope.pending.delete(key);
        paintBusy(button, scope);
      } else {
        delete button.dataset.busy;
        button.disabled = button.dataset.blocked === 'true';
      }
    }
  }

  // actOwner runs an owner command whose accepted outcome goes into the feed
  // of the instance the command was taken on, in place of the shared result
  // line: done's text is that outcome. A command taken where no feed is shown,
  // such as on the project pane, keeps its outcome in the result line.
  // Sending…, a refusal and the card restore behave as act's do.
  function actOwner(button, call, done, refused) {
    const feed = ui.view === 'workstream' || ui.view === 'workgroup';
    return act(inboxResult, button, call, (out) => feed ? { feed: done(out) } : done(out), refused);
  }

  let engineStatusRead = 0;

  async function readEngine(opening) {
    const reading = ++engineStatusRead;
    const box = byId('engine-status');
    box.replaceChildren(stateBlock('loading', 'Reading engine status…'));
    try {
      const status = await request('GET', '/engine/status');
      if (opening !== engineOpening || reading !== engineStatusRead) return;
      const field = (name, label, value) => el('p', {}, label + ': ', el('span', { 'data-field': name }, value));
      box.replaceChildren(...[
        field('mode', 'Mode', status.mode),
        status.address ? field('address', 'Address', status.address) : null,
        status.docker_host ? field('docker-host', 'Docker host', status.docker_host) : null,
        field('reachable', 'Reachability', status.reachable ? 'Reachable' : 'Unreachable'),
        status.reachability_error ? field('reachability-error', 'Reason', status.reachability_error) : null,
        field('version', 'Engine version', status.version || 'Unknown'),
        field('expected-version', 'Configured version', status.expected_version || 'Unknown'),
        field('cli-version', 'Host CLI version', status.cli_version || 'Unknown'),
        field('mismatch', 'Version mismatch', [
          status.version_mismatch ? 'Engine differs from host CLI' : '',
          status.configured_version_mismatch ? 'Running engine differs from configured version' : ''
        ].filter(Boolean).join('; ') || 'None'),
        field('cache-size', 'Cache size', status.cache_size_bytes == null ? 'Unknown' : status.cache_size_bytes.toLocaleString() + ' bytes')].filter(Boolean));
    } catch (err) {
      if (opening === engineOpening && reading === engineStatusRead) {
        const again = el('button', { type: 'button', class: 'secondary', 'data-action': 'retry' }, 'Try again');
        again.addEventListener('click', () => readEngine(engineOpening));
        box.replaceChildren(stateBlock('error', 'The engine status could not be read.', err.message, again));
      }
    }
  }

  // openEngine starts a new opening of the Dagger engine section. Each
  // opening owns its outcomes; results from earlier openings are discarded.
  function openEngine() {
    engineOpening++;
    for (const action of ['restart', 'prune']) outcomeScopes.get(byId('engine-' + action + '-result')).entries.clear();
    renderOutcomes();
    readEngine(engineOpening);
  }

  function setupEngine() {
    for (const action of ['restart', 'prune']) {
      byId('engine-' + action).addEventListener('click', () => {
        const opening = engineOpening;
        act(byId('engine-' + action + '-result'), byId('engine-' + action),
          () => request('POST', '/engine/' + action), () => {
            if (opening === engineOpening) readEngine(opening);
            return action === 'restart' ? 'Engine restarted.' : 'Engine cache pruned.';
          });
      });
    }
  }

  // block disables a button while blocked is true, or while its action
  // runs.
  function block(button, blocked) {
    button.dataset.blocked = String(blocked);
    button.disabled = blocked || button.dataset.busy === 'true';
  }

  // setOptions replaces a select's options when they changed, keeping the
  // chosen value while it is still offered.
  function setOptions(select, options) {
    const key = JSON.stringify(options);
    if (select.dataset.options === key) {
      return;
    }
    const chosen = select.multiple ? [...select.selectedOptions].map(o => o.value) : select.value;
    select.dataset.options = key;
    select.replaceChildren(...options.map(([value, label]) => el('option', { value }, label)));
    if (select.multiple) {
      for (const option of select.options) option.selected = chosen.includes(option.value);
    } else if (options.some(([value]) => value === chosen)) {
      select.value = chosen;
    }
  }

  // place makes nodes the children of box, moving nothing when they already
  // are, so a field being typed in keeps its focus.
  function place(box, nodes) {
    const current = [...box.children];
    if (current.length !== nodes.length || current.some((node, i) => node !== nodes[i])) {
      box.replaceChildren(...nodes);
    }
  }

  function when(iso) {
    const at = new Date(iso);
    return Number.isNaN(at.getTime()) ? iso : at.toLocaleString();
  }

  function duration(seconds) {
    if (seconds < 60) {
      return seconds + 's';
    }
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) {
      return minutes + 'm';
    }
    return Math.floor(minutes / 60) + 'h ' + (minutes % 60) + 'm';
  }

  function scope(target) {
    switch (target.scope) {
      case 'factory':
        return 'The factory';
      case 'project':
        return repoLabel(target.project);
      case 'workstream':
        return 'Workstream ' + target.workstream;
      case 'role':
        return 'Role ' + target.role;
      default:
        return target.scope;
    }
  }

  function projects() {
    return views.config ? views.config.projects || [] : [];
  }

  function configuredRepos() {
    return projects().map(p => ({ ...p, label: p.name }));
  }

  function projectName(id) {
    const p = projects().find((p) => p.id === id);
    return p?.name || id;
  }

  // archiveLabel says who archived a workstream and how far its cleanup is.
  function archiveLabel(w) {
    return (w.archived_by === 'chief_of_staff' ? 'Archived by the chief of staff' : 'Archived') + ', ' + (w.trace_deleted ? 'trace deleted' : 'cleanup pending');
  }
  function repoLabel(id) {
    return configuredRepos().find(r => r.id === id)?.label || id;
  }

  function goal(w) {
    return w.status ? w.status.goal : w.workstream;
  }

  // viewLabel names a view the way the owner thinks of it, for the banner
  // that says a read failed.
  function viewLabel(name) {
    const fixed = {
      status: 'the workstream list',
      runtime: 'pauses and priorities',
      config: 'the configuration',
      inbox: 'the inbox',
      charter: 'charter proposals',
      activity: 'backend activity',
    };
    if (fixed[name]) return fixed[name];
    const tail = name.split('/');
    if (tail[0] === 'feed') return 'the conversation of ' + workstreamName(tail[1]);
    if (tail[0] === 'workgroups') return 'the shared conversation';
    if (tail[0] === 'packet') return 'the ratification packet of ' + workstreamName(tail[1]);
    if (tail[0] === 'delivery') return 'the final report of ' + workstreamName(tail[1]);
    if (tail[0] === 'design') return 'the design session ' + tail[1];
    if (tail[0] === 'sessions') return 'the changes of session ' + tail[1];
    if (tail[0] === 'projects' && tail[1] === 'charter') return 'the project charter';
    if (tail[0] === 'projects' && tail[1] === 'config') return 'the project settings';
    return name;
  }

  // retryReads reads the named views again, forgetting the failure each
  // one showed so the page says it is reading until the answer arrives.
  function retryReads(names) {
    for (const name of names) {
      delete failures[name];
    }
    render();
    mark(names);
  }

  // retryButton is the next step an error state offers.
  function retryButton(names, label = 'Try again') {
    const button = el('button', { type: 'button', class: 'secondary', 'data-action': 'retry' }, label);
    button.addEventListener('click', () => retryReads(names));
    return button;
  }

  // openButton is the next step an empty state offers: it opens a view.
  function openButton(label, view, primary = false) {
    const button = el('button', { type: 'button', class: primary ? 'primary' : 'secondary', 'data-open': view }, label);
    button.addEventListener('click', () => openView(view));
    return button;
  }

  // stateBlock builds a designed empty, loading or error state: a mark, what
  // is going on in a sentence, a line saying what to do next, and the
  // actions that do it. A loading state holds the room its content takes so
  // the page does not jump when the content arrives.
  function stateBlock(kind, title, detail, ...actions) {
    return el('div', { class: 'state', 'data-state': kind, role: kind === 'error' ? 'alert' : 'status' },
      kind === 'loading'
        ? el('span', { class: 'state-bars', 'aria-hidden': 'true' }, el('span'), el('span'), el('span'))
        : el('span', { class: 'state-mark', 'aria-hidden': 'true' }),
      el('p', { class: 'state-title' }, title),
      detail ? el('p', { class: 'state-detail' }, detail) : null,
      actions.length > 0 ? el('div', { class: 'actions' }, ...actions) : null);
  }

  // keptState returns the same state node for a key, so a list that places
  // its nodes on every render does not rebuild a state and restart its
  // motion.
  const stateNodes = new Map();
  function keptState(key, build) {
    if (!stateNodes.has(key)) stateNodes.set(key, build());
    return stateNodes.get(key);
  }

  // readState is the state of a view that has not arrived: reading while the
  // read is in flight, and an error with a retry once it failed.
  function readState(name, reading, failed, detail) {
    return failures[name]
      ? stateBlock('error', failed, detail || 'Check the connection, then read it again.', retryButton([name]))
      : stateBlock('loading', reading);
  }

  function renderProblems() {
    const failed = Object.entries(failures);
    const diagnostics = [];
    for (const name of ['status', 'runtime']) {
      const view = views[name];
      if (view && Array.isArray(view.diagnostics)) {
        for (const d of view.diagnostics) {
          diagnostics.push(d.field + ': ' + d.message);
        }
      }
    }
    byId('problems-title').textContent = failed.length > 0
      ? (failed.length === 1 ? 'One view could not be read.' : failed.length + ' views could not be read.')
      : 'The service reports a problem with its settings.';
    byId('problems-retry').hidden = failed.length === 0;
    byId('problems-retry').dataset.names = JSON.stringify(failed.map(([name]) => name));
    byId('problem-list').replaceChildren(
      ...failed.map(([name, message]) => el('li', { 'data-view-name': name }, el('span', {}, 'Could not read ' + viewLabel(name) + '.'), ' ', el('span', { class: 'meta' }, message))),
      ...diagnostics.map((text) => el('li', {}, text)));
    byId('problems').hidden = failed.length + diagnostics.length === 0;
  }

  // resume clears a pause the way osmia resume does.
  function resume(p, button) {
    act(byId('pause-result'), button, () => request('DELETE', '/runtime/pause', p.target),
      () => scope(p.target) + ' resumed.');
  }

  function renderPauses() {
    const list = byId('pause-list');
    if (!views.runtime) {
      list.replaceChildren();
      return;
    }
    const pauses = views.runtime.effective.pauses || [];
    if (pauses.length === 0) {
      list.replaceChildren(el('li', { class: 'empty' }, 'Nothing is paused.'));
      return;
    }
    list.replaceChildren(...pauses.map((p) => {
      // A role pause follows a provider limit; clearing the limit ends it.
      let button = null;
      if (p.target.scope !== 'role') {
        button = el('button', { type: 'button', 'data-field': 'resume' }, 'Resume');
        button.addEventListener('click', () => resume(p, button));
      }
      return el('li', { class: 'pause', 'data-pause': p.target.scope + ':' + (p.target.workstream || p.target.project || p.target.role || '') },
        el('div', { class: 'line' },
          el('strong', { 'data-field': 'scope' }, scope(p.target)),
          el('span', { class: 'tag', 'data-field': 'mode' }, p.mode)),
        el('div', { 'data-field': 'reason' }, p.reason),
        el('div', { class: 'meta' }, 'Set by ', el('span', { 'data-field': 'source' }, pauseSources[p.source] || p.source), ' at ', when(p.set_at)),
        button);
    }));
  }

  // pauseTargets are the scopes the pause form offers: the factory, the
  // active repos and each repo's workstreams.
  function pauseTargets() {
    const targets = [['factory', 'The factory', { scope: 'factory' }]];
    for (const p of configuredRepos()) {
      targets.push(['project:' + p.id, repoLabel(p.id), { scope: 'project', project: p.id }]);
      for (const w of views.status ? views.status.workstreams.filter((w) => w.project === p.id && !w.archived) : []) {
        targets.push(['workstream:' + w.workstream, repoLabel(p.id) + ': ' + goal(w),
          { scope: 'workstream', project: p.id, workstream: w.workstream }]);
      }
    }
    return targets;
  }

  function renderPauseForm() {
    setOptions(byId('pause-form').elements.target, [['', 'Choose what to pause'], ...pauseTargets()]);
  }

  function pause(event) {
    event.preventDefault();
    const form = byId('pause-form');
    const result = byId('pause-result');
    const selected = pauseTargets().find(([value]) => value === form.elements.target.value);
    const mode = form.elements.mode.value;
    const reason = form.elements.reason.value.trim();
    if (!selected) {
      show(result, 'error', 'Choose what to pause.');
      return;
    }
    const target = selected[2];
    if (reason === '') {
      show(result, 'error', 'Give a reason for the pause.');
      return;
    }
    act(result, event.submitter || form.querySelector('button'), () => request('PUT', '/runtime/pause', { target, mode, reason }), () => {
      form.elements.reason.value = '';
      return scope(target) + ' paused (' + mode + ').';
    });
  }

  // activityWorkstream names the workstream an activity entry concerns, from
  // the workstream list when it still lists one, and its raw ID otherwise.
  function activityWorkstream(id) {
    const w = streams().find((w) => w.workstream === id);
    return w ? goal(w) : id;
  }

  // activityBlocked reports whether an activity entry is shown as blocked:
  // failed, or, for a kind whose reporting is expected to keep moving, its
  // last progress is at least activityStallMs old, the same threshold and
  // the same per-kind rule the service's own health check uses.
  function activityBlocked(e) {
    return !!e.failure ||
      (activityStallTrackedKinds.has(e.kind) && Date.now() - new Date(e.last_progress).getTime() >= activityStallMs);
  }

  // renderActivity shows the backend activity record's popover list: every
  // operation in flight or failed, including one that is blocked or has
  // failed.
  function renderActivity() {
    const list = byId('activity-list');
    const entries = views.activity ? views.activity.activity : [];
    if (entries.length === 0) {
      list.replaceChildren(el('li', { class: 'empty' }, 'Nothing is running.'));
      return;
    }
    list.replaceChildren(...entries.map((e) => {
      const blocked = activityBlocked(e);
      const where = [e.workstream ? activityWorkstream(e.workstream) : null, e.project ? repoLabel(e.project) : null].filter((s) => s);
      return el('li', { class: 'activity-entry', 'data-activity': e.id, 'data-blocked': String(blocked) },
        el('div', { class: 'line' },
          el('strong', { 'data-field': 'kind' }, e.kind),
          where.length > 0 ? el('span', { class: 'meta-row', 'data-field': 'where' }, ...where.flatMap((part, i) => i > 0 ? [' ', el('span', {}, part)] : [el('span', {}, part)])) : null),
        el('div', { 'data-field': 'phase' }, e.phase + (e.progress !== null && e.progress !== undefined ? ' (' + Math.round(e.progress * 100) + '%)' : '')),
        e.failure ? el('div', { class: 'failure', 'data-field': 'failure' }, e.failure) : null,
        el('div', { class: 'meta meta-row' }, el('span', {}, 'Since ', when(e.started_at)), blocked ? ' ' : null, blocked ? el('span', { class: 'blocked-note', 'data-field': 'blocked' }, 'No progress since ' + when(e.last_progress)) : null));
    }));
  }

  // A pool is bounded when the service reports a numeric factory-wide limit.
  // The architect pool reports none (unbounded: at most one
  // turn per workstream), which is not a limit of 0.
  function boundedPool(r) {
    return typeof r.limit === 'number';
  }
  function poolUse(r) {
    return boundedPool(r) ? r.used + ' of ' + r.limit + ' slots' : r.used + ' active (unbounded; 1 per workstream)';
  }

  function renderCapacity() {
    const box = byId('capacity');
    if (!views.status) {
      box.replaceChildren();
      return;
    }
    const capacity = views.status.capacity;
    if (!capacity) {
      box.replaceChildren(el('p', { class: 'empty' }, 'Capacity is unavailable.'));
      return;
    }
    box.replaceChildren(
      ...capacity.roles.map((r) => el('div', { class: 'role', 'data-role': r.role },
        el('div', { class: 'line' },
          el('strong', {}, r.role),
          el('span', { 'data-field': 'slots' }, boundedPool(r) ? r.used + ' / ' + r.limit + ' slots' : r.used + ' active (unbounded; 1 per workstream)')),
        boundedPool(r) ? null : el('div', { class: 'meta', 'data-field': 'note' }, r.note || 'No factory-wide limit; at most one turn per workstream; no configuration setting.'),
        r.waiting.length === 0
          ? el('div', { class: 'meta' }, 'Nothing waits.')
          : el('ul', { class: 'waiting' }, ...r.waiting.map((w) => el('li', { 'data-field': 'waiting' },
            el('span', { class: 'id' }, w.workstream),
            ' ', w.unit ? 'unit ' + w.unit : w.review ? 'final review ' + w.review : w.preview ? 'preview' : w.session ? 'session' : 'agent ' + w.agent,
            ': ', waitReasons[w.reason] || w.reason))))),
      el('p', { class: 'meta', 'data-field': 'per-workstream' }, 'Each workstream may hold ' + capacity.per_workstream + ' worker slots. Check slots are shared separately across the factory.'));
  }

  function isMultiMember(w) {
    return (w?.workgroup?.members.length || 0) > 1;
  }

  function workgroups() {
    return [...new Map(streams().filter(w => w.workgroup?.members.length > 1).map(w => [w.workgroup.id, w.workgroup])).values()];
  }

  function groupMembers(id) {
    return streams().filter(w => w.workgroup?.id === id);
  }

  function groupFeed(id) {
    return 'workgroups/' + id + '/feed';
  }

  // groupLive tells whether any member of the workgroup is unarchived. Only
  // then can the owner write to it.
  function groupLive(id) {
    return groupMembers(id).some(w => !w.archived);
  }

  function groupGoal(id) {
    const latest = views[groupFeed(id)]?.entries.findLast(e => e.kind === 'status');
    return latest?.status.goal || groupMembers(id).find(w => !w.archived && w.status?.goal)?.status.goal ||
      groupMembers(id).find(w => w.status?.goal)?.status.goal || 'Workgroup';
  }

  function groupInbox(id) {
    const members = new Set(groupMembers(id).map(w => w.workstream));
    return (views.inbox?.entries || []).filter(e => e.kind !== 'project-creation' && (e.workgroup === id || (!e.workgroup && members.has(e.workstream))) && !dismissed.has(decisionKey(e)));
  }

  function groupProposals(id) {
    return groupMembers(id).flatMap(w => proposalsOf(w.workstream));
  }

  function groupWaiting(id) {
    return groupInbox(id).length + groupProposals(id).length;
  }

  function labelGroupDecision(node, text) {
    let label = node.querySelector('[data-group-decision-repo]');
    if (!label) {
      label = el('div', { class: 'meta', 'data-group-decision-repo': '' });
      node.prepend(label);
    }
    label.textContent = text;
    return node;
  }

  function groupActivity(id) {
    const feed = views[groupFeed(id)];
    return feed ? JSON.stringify(feed.entries.filter(groupEntry)) : null;
  }

  function selectWorkgroup(id, aboutMember = null) {
    if (!workgroups().some(g => g.id === id)) return;
    ui.workgroup = id;
    ui.aboutMember = aboutMember;
    ui.toEnd = true;
    openView('workgroup');
    syncRoute();
    mark([groupFeed(id)]);
  }

  function renderWorkgroupView() {
    if (ui.view !== 'workgroup') return;
    const id = ui.workgroup;
    const group = workgroups().find(g => g.id === id);
    const box = byId('workgroup-content');
    byId('workgroup-goal').textContent = groupGoal(id);
    const live = !!group && groupLive(id);
    // An all-archived workgroup is gone once its shared feed cannot be read.
    if (!group || (!live && failures[groupFeed(id)])) {
      box.replaceChildren(stateBlock('empty', 'This workgroup is unavailable.', 'It may have been archived. Open another workstream from the list.', openButton('Go to projects', 'project-pane')));
      return;
    }
    const c = card(id, true);
    const member = groupMembers(id).find(w => w.workstream === ui.aboutMember && !w.archived);
    // Without a live member the conversation is read only.
    c.form.hidden = !live;
    // block records the lock where the busy painting reads it, so a later
    // render of the outcomes keeps the send button disabled.
    c.text.disabled = !live;
    block(c.button, !live);
    if (!live) {
      c.text.value = '';
      if (!c.readOnly) c.readOnly = el('p', { class: 'meta', 'data-field': 'read-only' }, 'Every member is archived; this conversation is read only.');
      if (!c.readOnly.isConnected) c.form.before(c.readOnly);
    } else if (c.readOnly?.isConnected) {
      c.readOnly.remove();
    }
    c.about.replaceChildren();
    if (member) {
      const clear = el('button', { type: 'button', 'aria-label': 'Send to the whole workgroup' }, '×');
      clear.addEventListener('click', () => { ui.aboutMember = null; renderWorkgroupView(); c.text.focus(); });
      c.about.append(el('span', { class: 'tag', 'data-field': 'about' }, 'About: ' + repoLabel(member.project), ' ', clear));
    }
    const feed = views[groupFeed(id)];
    const outcomes = ownerOutcomes.get('workgroup:' + id) || [];
    const key = JSON.stringify([feed, group.edges, group.dropped, views.config, outcomes]);
    if (c.shown !== key) {
      c.shown = key;
      const entries = (feed?.entries || []).filter(groupEntry);
      const latest = entries.findLastIndex(e => e.kind === 'status');
      c.entries.replaceChildren(...(feed ? [el('ol', {}, ...withOutcomes(entries, outcomes, (e, i) => {
        const node = e.kind === 'status' ? renderStatus(e, i === latest) : e.kind === 'brief' ? renderBriefEntry(e) : renderConversationEntry(e);
        if (e.repo) node.prepend(el('span', { class: 'tag', 'data-field': 'repo' }, repoLabel(e.repo)));
        return node;
      }))] : [readState(groupFeed(id), 'Reading the conversation…', 'The conversation is unavailable.'),
        ...(outcomes.length ? [el('ol', {}, ...outcomes.map(renderOwnerOutcome))] : [])]));
      const members = el('div', { class: 'workgroup-members', 'data-field': 'members' }, ...(feed?.members || []).map(m => {
        const button = el('button', { type: 'button', class: 'member-open', 'data-open-member': m.workstream },
          el('strong', { 'data-field': 'repo' }, repoLabel(m.repo)),
          el('span', { class: 'tag', 'data-field': 'state' }, m.display_state || m.state || 'planning'),
          m.archived ? el('span', { class: 'tag', 'data-field': 'archived' }, m.archived_by === 'chief_of_staff' ? 'Archived by chief of staff' : 'Archived') : null,
          m.running ? el('span', { class: 'working', 'data-field': 'running', 'aria-label': 'Agent running' }) : null);
        button.disabled = m.archived;
        button.addEventListener('click', () => select(m.workstream));
        return el('section', { class: 'member-card', 'data-member': m.workstream }, button,
          memberTube(m.units),
          m.wait ? el('p', { 'data-field': 'wait' }, m.wait) : null,
          m.pr_link ? el('a', { href: m.pr_link, 'data-field': 'pr' }, 'Pull request') : null);
      }));
      const graph = group.edges?.length ? el('figure', { class: 'workgroup-graph', 'aria-label': 'Workstream graph' },
        el('figcaption', {}, 'Dependencies'), ...group.edges.map(e => edgeLine(group, e,
          repoLabel(groupMembers(id).find(w => w.workstream === e.from)?.project || e.from) + ' → ' +
          repoLabel(groupMembers(id).find(w => w.workstream === e.to)?.project || e.to) + ', ' + e.type.replaceAll('_', ' ')))) : null;
      if (!c.summary) {
        c.summary = el('div', { class: 'workgroup-summary' });
        c.node.prepend(c.summary);
        c.node.classList.add('workgroup-view');
        c.node.dataset.workgroup = id;
      }
      c.summary.replaceChildren(members, ...(graph ? [graph] : []));
    }
    const decisions = groupInbox(id).map(e => labelGroupDecision(renderDecision(e),
      e.workgroup ? 'Workgroup, ' + [...new Set(groupMembers(id).map(w => repoLabel(w.project)))].join(', ') : repoLabel(e.project)));
    const proposals = groupProposals(id).map(p => labelGroupDecision(renderProposal(p),
      repoLabel(groupMembers(id).find(w => w.workstream === p.workstream)?.project)));
    const cards = [...decisions, ...proposals];
    place(c.decisions, [...(cards.length > 0 ? [c.heading] : []), ...cards, inboxResult]);
    place(box, [c.node]);
  }

  function streams() {
    return views.status ? views.status.workstreams : [];
  }

  // shown is the ordinary workstream the main area shows: the selected one
  // while the status lists it as unarchived and not a session, and otherwise
  // the first such workstream in the list of work, which the status already
  // orders by last activity. Sessions have their own view and shownSession.
  function shown() {
    const list = streams().filter((w) => w.kind !== 'session' && w.kind !== 'design');
    return list.find((w) => !w.archived && w.workstream === ui.selected) || list.find((w) => !w.archived) || null;
  }

  // shownSession is the session the session view shows: the selected
  // workstream, when the status lists it as a session. Unlike shown, it
  // never falls back to another session, so the view is empty until one is
  // chosen.
  function shownSession() {
    return streams().find((w) => w.kind === 'session' && !w.archived && w.workstream === ui.selected) || null;
  }

  // sessionIds names every session this page has opened or seen listed, so
  // select can route to the session view the moment a session opens, before
  // the next status read lists it.
  const sessionIds = new Set();

  function isSessionId(id) {
    if (sessionIds.has(id)) {
      return true;
    }
    const w = views.status ? views.status.workstreams.find((x) => x.workstream === id) : undefined;
    return !!(w && w.kind === 'session');
  }

  function terminal(w) {
    return w.state === 'delivered' || w.state === 'abandoned';
  }

  // inboxOf lists the workstream's open entries, less any a submission has
  // dismissed from the feed; see dismissed above.
  function inboxOf(id) {
    const w = streams().find(w => w.workstream === id);
    const group = w?.workgroup?.id;
    // A stuck project creation waits on the project pane, not on a workstream.
    return views.inbox ? views.inbox.entries.filter((e) => e.kind !== 'project-creation' && (isMultiMember(w) ? !e.workgroup && e.kind !== 'escalation' && !e.answer?.body?.project_spec_revision && e.workstream === id : (e.workstream === id || (group && e.workgroup === group))) && !dismissed.has(decisionKey(e))) : [];
  }

  function proposalsOf(id) {
    return views.charter ? views.charter.proposals.filter((p) => p.workstream === id) : [];
  }

  // activity is what the owner has seen of a workstream once they look at
  // it: its status and what waits on them, and what Conversation shows, its
  // conversation and milestones. Sessions, unit changes and check runs, which
  // only Activity shows, are not activity here. It is null until the feed and
  // inbox are read.
  function activity(w) {
    const feed = views['feed/' + w.workstream];
    if (!feed || !views.inbox) {
      return null;
    }
    const entries = feedView(w.workstream).entries;
    const conversation = entries.filter((e) => !activityKinds.has(e.kind));
    const last = conversation[conversation.length - 1];
    return JSON.stringify([
      !isMultiMember(w) && w.status ? [w.status.goal, w.status.attention, w.status.note] : null,
      entries.filter(isMilestone).map(entryKey),
      inboxOf(w.workstream).map((e) => decisionKey(e) + ':' + e.revision),
      proposalsOf(w.workstream).map((p) => p.question),
      conversation.length,
      last ? last.state : null,
    ]);
  }

  // unread reports whether a workstream changed since the owner last looked
  // at it. One the page has never shown counts as unread unless the first
  // status read listed it.
  function unread(w) {
    const now = activity(w);
    if (now === null) {
      return false;
    }
    const id = w.workstream;
    if (!(id in ui.seen)) {
      if (ui.initial && ui.initial.has(id)) {
        ui.seen[id] = now;
        store('seen', ui.seen);
        return false;
      }
      return true;
    }
    return ui.seen[id] !== now;
  }

  // markSeen records the activity of the workstream the owner is looking at.
  function markSeen() {
    if (ui.view === 'workgroup') {
      const now = groupActivity(ui.workgroup);
      if (now !== null && document.visibilityState === 'visible' && document.body.dataset.picker !== 'open') {
        ui.seen['workgroup:' + ui.workgroup] = now;
        store('seen', ui.seen);
      }
    }
    const w = shown();
    if (!w || ui.view !== 'workstream' || document.visibilityState !== 'visible' || document.body.dataset.picker === 'open') {
      return;
    }
    const now = activity(w);
    if (now !== null && ui.seen[w.workstream] !== now) {
      ui.seen[w.workstream] = now;
      store('seen', ui.seen);
    }
  }

  // forget drops the preferences of workstreams the status no longer lists.
  function forget() {
    if (!views.status) {
      return;
    }
    const ids = new Set([...streams().map(w => w.workstream), ...workgroups().map(g => 'workgroup:' + g.id)]);
    if (!ui.initial) {
      ui.initial = ids;
    }
    const gone = Object.keys(ui.seen).filter((id) => !ids.has(id));
    if (gone.length > 0) {
      gone.forEach((id) => delete ui.seen[id]);
      store('seen', ui.seen);
    }
  }

  function pausedIn(w) {
    const pauses = views.runtime ? views.runtime.effective.pauses || [] : [];
    return pauses.filter((p) => (p.target.scope === 'workstream' && p.target.workstream === w.workstream) ||
      (p.target.scope === 'project' && p.target.project === w.project));
  }

  // rows keeps each workstream's row in the list between renders.
  const rows = new Map();
  const sidebarGroups = new Map();
  const archivedSidebarGroups = new Map();

  function row(id) {
    let r = rows.get(id);
    if (r) {
      return r;
    }
    r = {
      kind: el('span', { class: 'tag', 'data-field': 'kind' }, 'Session'),
      title: el('span', { class: 'title', 'data-field': 'goal' }),
      project: el('span', { 'data-field': 'project' }),
      state: el('span', { class: 'glyph', 'data-field': 'state', role: 'img' }),
      signals: el('span', { class: 'signals' }),
    };
    r.button = el('button', { type: 'button', class: 'row', 'data-select': id },
      r.state, el('span', { class: 'text' }, r.title, el('span', { class: 'sub meta-row' }, r.kind, r.project)), r.signals);
    r.button.addEventListener('click', () => select(id));
    r.node = el('li', {}, r.button);
    rows.set(id, r);
    return r;
  }

  // stateGlyph names the shape a workstream's state takes in the sidebar:
  // live while work runs, waiting before it starts, done once it is
  // assembled or delivered, failed once it stops without delivering, idle
  // for a session with nothing running and archived once its history is
  // gone. A shape accompanies the status colour, which never carries the
  // state alone.
  function stateGlyph(w) {
    if (w.archived) return 'archived';
    const state = w.state || 'handed';
    if (state === 'delivered' || state === 'assembled') return 'done';
    if (state === 'abandoned' || state === 'failed' || state === 'cancelled' || state === 'blocked') return 'failed';
    if (state === 'building' || (w.agents && w.agents.length > 0)) return 'live';
    if (w.kind === 'session' || w.kind === 'design') return 'idle';
    return 'waiting';
  }

  function renderSidebar() {
    const list = byId('workstream-list');
    if (!views.status) {
      place(list, [failures.status
        ? keptState('sidebar:error', () => el('li', { class: 'none', 'data-field': 'error' },
          stateBlock('error', 'The workstream list could not be read.', 'Check the connection, then read it again.', retryButton(['status']))))
        : keptState('sidebar:loading', () => el('li', { class: 'none', 'data-field': 'loading' }, stateBlock('loading', 'Loading workstreams…')))]);
      return;
    }
    const ids = new Set(streams().map((w) => w.workstream));
    for (const id of rows.keys()) {
      if (!ids.has(id)) {
        rows.delete(id);
      }
    }
    const current = streams().find((w) => !w.archived && w.workstream === ui.selected) || shown();
    const archived = streams().filter((w) => w.archived);
    const group = byId('archived');
    group.hidden = archived.length === 0;
    byId('archived-count').textContent = String(archived.length);
    const entry = (w) => {
      const r = row(w.workstream);
      const needs = inboxOf(w.workstream).length + proposalsOf(w.workstream).length;
      const paused = pausedIn(w).length > 0;
      r.kind.hidden = w.kind !== 'session' && w.kind !== 'design';
      r.kind.textContent = w.kind === 'design' ? 'Design' : 'Session';
      if (w.kind === 'session') {
        sessionIds.add(w.workstream);
      }
      if (w.kind === 'design' && !startedDesignIds.has(w.workstream)) {
        designIds.set(w.workstream, w);
      }
      const label = w.kind === 'session' || w.kind === 'design' ? (w.title || w.workstream) : isMultiMember(w) ? repoLabel(w.project) : goal(w);
      r.title.textContent = label;
      r.title.title = label;
      r.project.textContent = w.kind === 'design' ? projectName(w.project) : repoLabel(w.project);
      const stateName = w.archived ? archiveLabel(w).toLowerCase() : (w.state || 'handed');
      r.state.dataset.glyph = stateGlyph(w);
      r.state.setAttribute('aria-label', stateName);
      r.state.title = stateName;
      r.button.disabled = !!w.archived;
      r.button.title = w.archived ? 'Permanently archived; history is unavailable.' : '';
      r.button.setAttribute('aria-current', String(ui.view !== 'workgroup' && current !== null && current.workstream === w.workstream));
      r.signals.replaceChildren(...[
        paused ? el('span', { class: 'paused', 'data-field': 'paused', title: 'Paused' }, 'paused') : null,
        w.agents && w.agents.length > 0 ? el('span', { class: 'working', 'data-field': 'working', title: w.agents.length + ' running' }) : null,
        needs > 0 ? el('span', { class: 'needs', 'data-field': 'needs', title: needs + ' waiting for you', 'aria-label': needs + ' waiting for you' }, String(needs)) : null,
        needs === 0 && unread(w) ? el('span', { class: 'unread', 'data-field': 'unread', title: 'New activity' }) : null,
      ].filter((node) => node !== null));
      return r.node;
    };
    // A group's newest member determines its position. Keep member controls
    // connected across updates, including when a member is archived.
    const groupedEntries = (work, cache) => {
      const nodes = [];
      const members = new Map();
      for (const w of work) {
        if (!w.workgroup || w.workgroup.members.length < 2) {
          nodes.push(entry(w));
          continue;
        }
        const id = w.workgroup.id;
        if (!members.has(id)) {
          members.set(id, []);
          let group = cache.get(id);
          if (!group) {
            group = { list: el('ul', { class: 'rows' }), title: el('span', { class: 'title' }), signals: el('span', { class: 'signals' }) };
            group.button = el('button', { type: 'button', class: 'row sidebar-workgroup-title', 'data-select-workgroup': id }, group.title, group.signals);
            group.button.addEventListener('click', () => selectWorkgroup(id));
            group.node = el('li', { class: 'sidebar-workgroup', 'data-sidebar-workgroup': id },
              group.button, group.list);
            cache.set(id, group);
          }
          group.title.textContent = groupGoal(id);
          group.button.title = groupGoal(id);
          group.button.setAttribute('aria-current', String(ui.view === 'workgroup' && ui.workgroup === id));
          const needs = groupWaiting(id);
          const now = groupActivity(id);
          const key = 'workgroup:' + id;
          if (!(key in ui.seen) && ui.initial?.has(key) && now !== null) ui.seen[key] = now;
          group.signals.replaceChildren(...[
            needs ? el('span', { class: 'needs', 'data-field': 'needs' }, String(needs)) : null,
            now !== null && ui.seen[key] !== now ? el('span', { class: 'unread', 'data-field': 'unread', title: 'New conversation' }) : null,
          ].filter(Boolean));
          nodes.push(group.node);
        }
        members.get(id).push(entry(w));
      }
      for (const [id, group] of cache) {
        if (members.has(id)) place(group.list, members.get(id));
        else cache.delete(id);
      }
      return nodes;
    };
    place(byId('archived-list'), groupedEntries(archived, archivedSidebarGroups));
    const work = streams().filter((w) => !w.archived);
    if (work.length === 0) {
      place(list, [keptState(archived.length === 0 ? 'sidebar:empty' : 'sidebar:archived', () => el('li', { class: 'none', 'data-field': 'empty' },
        archived.length === 0
          ? stateBlock('empty', 'No workstreams yet.', 'Hand in a feature to start one.', openButton('Hand in a feature', 'handin'))
          : stateBlock('empty', 'Every workstream is archived.', 'Hand in a feature to start new work.', openButton('Hand in a feature', 'handin'))))]);
      return;
    }
    place(list, groupedEntries(work, sidebarGroups));
  }

  // renderNeedsYou lists every open inbox entry and charter proposal across
  // the workstreams, each linking to the view that answers it. Project
  // creations stay below as cards, where they are answered. The list is
  // built from the inbox and charter reads the page already makes. Each
  // item keeps its link between renders, so a focused link stays focused.
  const needsItems = new Map();
  const needsCount = el('span', { class: 'needs', 'data-field': 'needs-count' });
  const needsList = el('ul', { class: 'needs-list' });
  const needsEmpty = el('p', { class: 'meta', 'data-field': 'needs-empty' }, 'Nothing is waiting for you.');
  const needsSection = el('section', { class: 'needs-you', 'data-field': 'needs-you', 'aria-labelledby': 'needs-you-title' },
    el('div', { class: 'needs-head' }, el('h3', { id: 'needs-you-title' }, 'Needs you'), needsCount), needsList, needsEmpty);

  function needsItem(key) {
    let item = needsItems.get(key);
    if (item) return item;
    item = {
      kind: el('span', { class: 'needs-kind', 'data-field': 'kind' }),
      title: el('span', { class: 'needs-title', 'data-field': 'goal' }),
      text: el('span', { class: 'needs-text', 'data-field': 'question' }),
      project: el('span', { 'data-field': 'project' }),
      opened: el('span', { class: 'meta', 'data-field': 'opened' }),
      target: null,
    };
    item.link = el('a', { class: 'needs-link' }, item.kind, item.title, item.text,
      el('span', { class: 'needs-where meta-row' }, item.project, item.opened));
    item.link.addEventListener('click', event => {
      event.preventDefault();
      if (item.target.group) selectWorkgroup(item.target.group);
      else select(item.target.workstream);
    });
    item.node = el('li', {}, item.link);
    needsItems.set(key, item);
    return item;
  }

  function renderNeedsYou() {
    if (!views.inbox) return null;
    const entries = views.inbox.entries.filter(e => e.kind !== 'project-creation' && !dismissed.has(decisionKey(e)));
    const proposed = views.charter ? views.charter.proposals : [];
    const wanted = [
      ...entries.map(e => {
        const group = e.workgroup && workgroups().some(g => g.id === e.workgroup) ? e.workgroup : '';
        return { key: 'entry:' + decisionKey(e), kind: kinds[e.kind] || e.kind, title: group ? groupGoal(group) : workstreamName(e.workstream),
          text: e.question, project: repoLabel(e.project), opened: e.opened_at, target: { workstream: e.workstream, group } };
      }),
      ...proposed.map(r => ({ key: 'proposal:' + proposalKey(r), kind: 'Charter rule', title: workstreamName(r.workstream), text: r.rule,
        project: repoLabel(streams().find(w => w.workstream === r.workstream)?.project || ''), opened: '', target: { workstream: r.workstream, group: '' } })),
    ];
    const nodes = wanted.map(w => {
      const item = needsItem(w.key);
      item.target = w.target;
      item.kind.textContent = w.kind;
      item.title.textContent = w.title;
      item.text.textContent = w.text;
      item.project.textContent = w.project;
      item.opened.textContent = w.opened ? when(w.opened) : '';
      item.opened.hidden = !w.opened;
      item.link.href = w.target.group ? '#workgroup=' + encodeURIComponent(w.target.group) : '#workstream=' + encodeURIComponent(w.target.workstream);
      item.node.dataset.needsWorkstream = w.target.workstream;
      return item.node;
    });
    for (const key of needsItems.keys()) if (!wanted.some(w => w.key === key)) needsItems.delete(key);
    place(needsList, nodes);
    needsCount.textContent = String(nodes.length);
    needsCount.setAttribute('aria-label', nodes.length + ' waiting for you');
    needsCount.hidden = nodes.length === 0;
    needsList.hidden = nodes.length === 0;
    needsEmpty.hidden = nodes.length > 0;
    return needsSection;
  }

  let paneRendered = '';
  // Keep focused controls connected while status and inbox events update labels.
  const paneProjects = new Map();
  const paneGroups = new Map();
  const paneEntries = new Map();

  function renderProjectPane() {
    const key = JSON.stringify([projects(), streams(), views.inbox, views.charter, [...dismissed.keys()]]);
    if (key === paneRendered) return;
    paneRendered = key;
    const action = (label, view, project) => {
      const button = el('button', { type: 'button', 'data-project-action': view }, label);
      button.addEventListener('click', () => {
        openView(view, project);
      });
      return button;
    };
    const entry = (w) => {
      let row = paneEntries.get(w.workstream);
      if (!row) {
        row = {
          title: el('strong'), repo: el('span', { class: 'meta', 'data-field': 'repo' }),
          state: el('span', { class: 'tag', 'data-field': 'state' }),
          wait: el('p', { class: 'pane-wait', 'data-field': 'wait' }),
          needs: el('span', { class: 'needs' }), archive: el('p', { class: 'meta' }),
        };
        row.button = el('button', { type: 'button', class: 'pane-workstream', 'data-drilldown': w.workstream }, row.title, row.repo, row.state);
        row.button.addEventListener('click', () => select(w.workstream));
        row.node = el('li', { 'data-pane-workstream': w.workstream }, row.button, row.wait, row.needs, row.archive);
        paneEntries.set(w.workstream, row);
      }
      row.title.textContent = w.kind === 'session' || w.kind === 'design' ? (w.title || w.workstream) : goal(w);
      row.repo.textContent = w.kind === 'design' ? projectName(w.project) : repoLabel(w.project);
      row.state.textContent = w.display_state || w.state || w.kind || 'planning';
      row.button.disabled = !!w.archived;
      const needs = inboxOf(w.workstream).filter(e => !e.workgroup).length;
      row.wait.textContent = w.wait || '';
      row.wait.hidden = !w.wait;
      row.needs.textContent = needs + ' waiting for you';
      row.needs.hidden = !needs;
      row.archive.textContent = archiveLabel(w);
      row.archive.hidden = !w.archived;
      return row.node;
    };
    const projectNodes = projects().map(p => {
      const ids = new Set([p.id, ...(p.repos || []).map(r => r.id)]);
      const work = streams().filter(w => ids.has(w.project)).sort((a, b) => a.workstream.localeCompare(b.workstream));
      const groups = new Map();
      for (const w of work) {
        if (w.workgroup) groups.set(w.workgroup.id, w.workgroup);
      }
      const groupsView = [...groups.values()].map(g => {
        const members = work.filter(w => w.workgroup?.id === g.id);
        const graph = g.members.length > 1 || g.edges.length > 0;
        let row = paneGroups.get(p.id + ":" + g.id);
        if (!row) {
          row = { inbox: el('button', { type: 'button' }), members: el('ul', { class: 'pane-streams' }), graph: el('div'), graphKey: '' };
          row.open = el('button', { type: 'button', 'data-open-workgroup': g.id }, 'Workgroup');
          const open = () => {
            if (workgroups().some(group => group.id === g.id)) selectWorkgroup(g.id);
            else {
              const live = groupMembers(g.id).find(w => !w.archived);
              if (live) select(live.workstream);
            }
          };
          row.inbox.addEventListener('click', open);
          row.open.addEventListener('click', open);
          row.node = el('section', { class: 'pane-workgroup', 'data-workgroup': g.id },
            el('h4', {}, row.open), el('p', { class: 'id' }, g.id), row.inbox, row.members, row.graph);
          paneGroups.set(p.id + ":" + g.id, row);
        }
        const waiting = g.members.length > 1 ? groupInbox(g.id) : (views.inbox?.entries || []).filter(e => e.workgroup === g.id && !dismissed.has(decisionKey(e)));
        row.open.disabled = g.members.length < 2 && !members.some(w => !w.archived);
        const waitingCount = waiting.length + (g.members.length > 1 ? groupProposals(g.id).length : 0);
        row.inbox.textContent = waitingCount + ' waiting for you, open inbox';
        row.inbox.hidden = !waitingCount;
        if (waitingCount) row.inbox.dataset.groupInbox = g.id;
        else delete row.inbox.dataset.groupInbox;
        place(row.members, members.map(entry));
        const graphKey = JSON.stringify([g.members, g.edges, g.dropped, p.repos]);
        if (row.graphKey !== graphKey) {
          row.graphKey = graphKey;
          row.graph.replaceChildren(...(graph ? [el('figure', { class: 'workgroup-graph', 'aria-label': 'Workstream graph' },
            el('figcaption', {}, 'Workstream graph'),
            el('ul', {}, ...g.members.map(id => el('li', {},
              metaRow(repoLabel(streams().find(w => w.workstream === id)?.project || id), el('code', { class: 'id' }, id))))),
            ...g.edges.map(e => edgeLine(g, e, e.from + ' → ' + e.to + ', ' + e.type.replaceAll('_', ' '))))] : []));
        }
        return row.node;
      });
      let row = paneProjects.get(p.id);
      if (!row) {
        row = { name: el('h3'), repos: el('ul', { class: 'pane-repos' }), groups: el('div'),
          none: el('p', { class: 'meta' }, 'No workgroups yet.'), ungrouped: el('ul', { class: 'pane-streams' }), sessions: [] };
        const sections = [['design', 'Design sessions'], ['session', 'Exploratory sessions']].map(([kind, title]) => {
          const list = el('ul', { class: 'pane-streams' });
          const node = el('section', { 'data-sessions': kind }, el('h4', {}, title), list);
          row.sessions.push({ kind, list, node });
          return node;
        });
        row.node = el('article', { class: 'project-card', 'data-project': p.id }, row.name,
          el('div', { class: 'actions' }, action('Open a design session', 'design-add', p.id),
            action('Hand in a feature', 'handin', p.id),
            action('Project management', 'projects', p.id), action('Open an exploratory session', 'session-add', p.id)),
          el('h4', {}, 'Repos'), row.repos, el('h4', {}, 'Workgroups'), row.groups, row.none, row.ungrouped, ...sections);
        paneProjects.set(p.id, row);
      }
      row.name.textContent = p.name;
      const repoKey = JSON.stringify([p.name, p.repos]);
      if (row.repoKey !== repoKey) {
        row.repoKey = repoKey;
        row.repos.replaceChildren(...(p.repos || []).map(r => el('li', { 'data-repo': r.id, class: 'meta-row' }, el('strong', {}, repoLabel(r.id)), ' ', el('span', { class: 'meta' }, r.upstream))));
      }
      place(row.groups, groupsView);
      row.none.hidden = !!groupsView.length;
      place(row.ungrouped, work.filter(w => !w.workgroup && !w.kind).map(entry));
      row.ungrouped.hidden = !row.ungrouped.children.length;
      for (const section of row.sessions) {
        place(section.list, work.filter(w => w.kind === section.kind && !w.workgroup).map(entry));
        section.node.hidden = !section.list.children.length;
      }
      return row.node;
    });
    // A creation that cannot finish waits for the owner here, whether a start,
    // the CLI or the API began it.
    const creations = (views.inbox ? views.inbox.entries : []).filter((e) => e.kind === 'project-creation' && !dismissed.has(decisionKey(e))).map(renderDecision);
    place(byId('project-pane'), [renderNeedsYou(), ...(creations.length ? [el('h3', { 'data-field': 'creations' }, 'Project creation waiting for you'), ...creations, inboxResult] : []), ...projectNodes].filter(node => node !== null));
    if (!projects().length) {
      byId('project-pane').append(views.config
        ? stateBlock('empty', 'No projects yet.', 'Create a project to register its repository, then hand in a feature.', openButton('Create project', 'project-add', true))
        : failures.config
          ? stateBlock('error', 'The projects could not be read.', 'Check the connection, then read them again.', retryButton(['config']))
          : stateBlock('loading', 'Loading projects…'));
    }
    for (const id of paneProjects.keys()) if (!projects().some(p => p.id === id)) paneProjects.delete(id);
    for (const id of paneGroups.keys()) if (!streams().some(w => w.project + ":" + w.workgroup?.id === id)) paneGroups.delete(id);
    for (const id of paneEntries.keys()) if (!streams().some(w => w.workstream === id)) paneEntries.delete(id);
  }

  // setPicker opens or closes the workstream list at phone widths, where it
  // covers the page until the owner picks a workstream or closes it.
  function setPicker(open) {
    document.body.dataset.picker = open ? 'open' : 'closed';
    byId('picker-toggle').setAttribute('aria-expanded', String(open));
  }

  // select shows a workstream or session in the main area and marks it as
  // the current selection, without changing the sidebar order.
  function select(id) {
    const changed = ui.selected !== id;
    ui.selected = id;
    store('selected', id);
    setPicker(false);
    const session = isSessionId(id);
    const design = !session && isDesignId(id);
    ui.toEnd = true;
    if (changed) {
      ui.unit = '';
      ui.open.clear();
      ui.focus = '';
      ui.scrollFocus = false;
    }
    openView(session ? 'session' : design ? 'design' : 'workstream');
    syncRoute();
    if (changed) {
      clearWorkstreamForms();
    }
    if (streams().some((w) => w.workstream === id && w.archived)) {
      byId('archived').open = true;
    }
    render();
    if (changed && !views['feed/' + id] && !design) {
      mark(['feed/' + id]);
    }
    if (changed && ui.tab === 'documents') {
      readWorkstreamDocuments();
    }
    if (session) {
      mark(['status', 'sessions/' + id + '/changes']);
    }
    if (design) {
      mark(['status', 'design/' + id, 'design/' + id + '/conversation']);
    }
  }

  // openView shows a view in the main area: the selected workstream or
  // session, or one of the views the header opens.
  function openView(view, project = '') {
    ui.navigated = true;
    for (const popover of document.querySelectorAll('[popover]')) {
      if (popover.matches(':popover-open')) {
        popover.hidePopover();
      }
    }
    const section = settingsSections.includes(view) ? view : view === 'settings' ? ui.section : null;
    if (['handin', 'design-add', 'session-add'].includes(view) || (view === 'projects' && project)) {
      const form = document.querySelector('[data-view="' + view + '"] form');
      const changedProject = form.elements.project.value !== project;
      form.elements.project.value = project;
      if (view === 'handin') handinProjects = project ? [project] : [];
      if (view !== 'projects' || changedProject) form.elements.project.dispatchEvent(new Event('change', { bubbles: true }));
    }
    if (section) view = 'settings';
    const changed = ui.view !== view;
    if (changed && ['project-pane', 'workstream', 'workgroup', 'session', 'design'].includes(ui.view)) ui.returnView = ui.view;
    const engineOpened = section === 'engine' && (changed || ui.section !== 'engine');
    ui.view = view;
    if (section) ui.section = section;
    if (changed) ui.toEnd = true;
    if (view === 'project-pane') history.replaceState(null, '', location.pathname + location.search);
    setPicker(false);
    render();
    if (engineOpened) openEngine();
    if ((changed || section) && !shownFeed()) byId('main').scrollTop = 0;
  }

  // setTab shows a tab of the workstream. Leaving Activity drops the entry
  // a link pointed at, and a tab other than Activity carries no filter or
  // opened entries in the location.
  function setTab(tab) {
    const changed = ui.tab !== tab;
    ui.tab = tab;
    if (changed) {
      ui.focus = '';
      ui.scrollFocus = false;
      if (tab === 'conversation' || tab === 'activity') ui.toEnd = true;
      syncRoute(true);
    }
    render();
    if (changed && tab === 'documents') {
      readWorkstreamDocuments();
    }
  }

  // showActivity opens Activity, optionally on one unit's events or at one
  // entry. The entry is opened, highlighted and scrolled to.
  function showActivity({ unit = '', entry = '' } = {}) {
    ui.unit = unit;
    ui.focus = entry;
    ui.scrollFocus = entry !== '';
    ui.tab = 'activity';
    ui.toEnd = entry === '';
    syncRoute(true);
    render();
  }

  // restoreRoute shows what the location hash names, after Back, Forward or
  // an edit of the hash.
  function restoreRoute() {
    const route = parseRoute(location.hash);
    if (route.kind === 'workgroup') {
      if (ui.view !== 'workgroup' || ui.workgroup !== route.id) selectWorkgroup(route.id);
      return;
    }
    if (route.kind !== 'workstream') return;
    if (ui.selected !== route.id || ui.view === 'project-pane' || ui.view === 'settings') select(route.id);
    if (ui.view !== 'workstream') return;
    const activity = route.tab === 'activity';
    ui.tab = route.tab;
    ui.unit = activity ? route.unit : '';
    ui.open = new Set(activity ? route.open : []);
    ui.focus = activity ? route.entry : '';
    ui.scrollFocus = ui.focus !== '';
    ui.toEnd = ui.focus === '' && (route.tab === 'conversation' || activity);
    syncRoute();
    render();
    if (route.tab === 'documents') readWorkstreamDocuments();
  }

  // inboxResult shows the outcome of the last answer to an inbox entry. It
  // moves to the card of the workstream shown, so it is held here rather
  // than looked up in the document.
  const inboxResult = byId('inbox-result');

  // cards keeps each workstream's card between renders, so a message being
  // written survives the events that arrive meanwhile and a switch to
  // another workstream and back.
  const cards = new Map();

  const dictationComposers = new Map();

  function dictationHTTPSAddress() {
    const tailnet = views.status?.tailnet;
    // The web listener accepts only localhost hosts; a non-localhost page
    // served by Osmia comes through the tailnet listener.
    if (window.isSecureContext || location.protocol !== 'http:' ||
        location.hostname === 'localhost' || location.hostname.endsWith('.localhost') ||
        tailnet?.https_state !== 'up' || !tailnet.https_address) return '';
    const address = new URL(tailnet.https_address);
    address.pathname = location.pathname;
    address.search = location.search;
    address.hash = location.hash;
    return address.href;
  }

  // recordingTypes are the audio formats the page asks MediaRecorder for, in
  // order of preference; the first the browser supports is used.
  const recordingTypes = ['audio/webm;codecs=opus', 'audio/mp4', 'audio/ogg;codecs=opus', 'audio/webm', 'audio/ogg'];

  // transcriptionOn says whether the service has a Whisper server configured.
  function transcriptionOn() {
    return !!views.config?.effective?.transcribe?.url;
  }

  // recordingAvailable says whether a composer can offer the microphone:
  // transcription is configured and the browser records audio, which it
  // allows only in a secure context.
  function recordingAvailable() {
    return window.isSecureContext && transcriptionOn() &&
      !!navigator.mediaDevices?.getUserMedia && typeof window.MediaRecorder === 'function';
  }

  function renderDictationAvailability(button, link) {
    button.hidden = !recordingAvailable();
    const address = dictationHTTPSAddress();
    link.hidden = !address;
    if (address) link.href = address;
    else link.removeAttribute('href');
  }

  function stopTracks(stream) {
    for (const track of stream ? stream.getTracks() : []) track.stop();
  }

  // blobBase64 reads a recording as standard base64 without a data: prefix.
  function blobBase64(blob) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => {
        const url = String(reader.result);
        resolve(url.slice(url.indexOf(',') + 1));
      };
      reader.onerror = () => reject(reader.error || new Error('The recording could not be read.'));
      reader.readAsDataURL(blob);
    });
  }

  // transcriptGap is the whitespace between a draft and the transcript
  // appended after it: none for an empty draft or one already ending in
  // whitespace.
  function transcriptGap(draft) {
    return draft === '' || /\s$/.test(draft) ? '' : ' ';
  }

  function microphoneMessage(err) {
    switch (err && err.name) {
      case 'NotAllowedError':
      case 'SecurityError':
        return 'Microphone permission denied.';
      case 'NotFoundError':
      case 'OverconstrainedError':
        return 'No microphone was found.';
      default:
        return 'Could not start recording: ' + (err && err.message ? err.message : 'the microphone is unavailable.');
    }
  }

  const transcribingText = 'Transcribing…';

  // clearOutcome removes the outcome a captured target holds.
  function clearOutcome(target) {
    if (!target.scope) return;
    target.scope.entries.delete(target.key);
    paintOutcome(target.scope);
  }

  // setupDictation gives a composer its microphone. One recording at a time
  // belongs to the composer and the conversation that started it, and stays
  // current through its upload; every callback of the recorder and of the
  // upload first checks that its recording is still current, so a stopped,
  // sent or abandoned recording can no longer change any draft or outcome.
  function setupDictation(form, result, owner) {
    const button = form.querySelector('[aria-label="Dictate"]');
    const link = el('a', { class: 'dictation-https', hidden: true }, 'Use HTTPS for dictation');
    button.before(link);
    const text = form.elements.text;
    const submit = form.querySelector('[type=submit]');
    let current = null;
    button.setAttribute('aria-pressed', 'false');
    function paint() {
      const phase = current ? current.phase : '';
      button.setAttribute('aria-pressed', String(phase === 'starting' || phase === 'recording'));
      block(button, text.disabled || submit.disabled || phase === 'stopping' || phase === 'transcribing');
    }
    function active(session) {
      return current === session && !session.cancelled && owner() === session.owner;
    }
    // end retires a recording: the recorder and the microphone stop, and
    // nothing the recording reports later is accepted.
    function end(session) {
      session.cancelled = true;
      if (current === session) current = null;
      try {
        if (session.recorder && session.recorder.state !== 'inactive') session.recorder.stop();
      } catch (_) { /* The browser may already have ended it. */ }
      stopTracks(session.stream);
      paint();
    }
    function fail(session, message) {
      if (!active(session)) return;
      end(session);
      show(session.target, 'error', message);
    }
    // finish abandons the current recording, taking back its progress
    // message while that still stands.
    function finish() {
      const session = current;
      if (!session) {
        paint();
        return;
      }
      end(session);
      const scope = session.target.scope;
      const shown = scope && scope.entries.get(session.target.key);
      if (shown && shown.outcome === 'pending' && shown.text === transcribingText) clearOutcome(session.target);
    }
    function render() {
      renderDictationAvailability(button, link);
      paint();
      if (current && (!active(current) || !form.isConnected || button.hidden || text.disabled || submit.disabled)) finish();
    }
    async function transcribe(session) {
      if (!active(session) || session.phase === 'transcribing') return;
      session.phase = 'transcribing';
      show(session.target, 'pending', transcribingText);
      paint();
      stopTracks(session.stream);
      const blob = new Blob(session.chunks, { type: session.chunks[0]?.type || session.type || 'audio/webm' });
      if (blob.size === 0) {
        fail(session, 'Nothing was recorded. Try again.');
        return;
      }
      try {
        const audio = await blobBase64(blob);
        if (!active(session)) return;
        const out = await request('POST', '/transcribe', { mime_type: blob.type, audio });
        if (!active(session)) return;
        const transcript = out && typeof out.text === 'string' ? out.text.trim() : '';
        if (transcript === '') {
          fail(session, 'No speech was detected. Try again.');
          return;
        }
        end(session);
        const at = text.value.length;
        text.setRangeText(transcriptGap(text.value) + transcript, at, at, 'end');
        clearOutcome(session.target);
      } catch (err) {
        fail(session, err.message);
      }
    }
    async function begin() {
      const key = owner();
      if (!key) return;
      const session = { owner: key, target: captureOutcome(result), phase: 'starting', chunks: [], stream: null, recorder: null, type: '' };
      current = session;
      paint();
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        session.stream = stream;
        if (!active(session)) {
          stopTracks(stream);
          return;
        }
        const type = recordingTypes.find(t => MediaRecorder.isTypeSupported ? MediaRecorder.isTypeSupported(t) : false) || '';
        const recorder = type ? new MediaRecorder(stream, { mimeType: type }) : new MediaRecorder(stream);
        session.recorder = recorder;
        session.type = type || recorder.mimeType || '';
        recorder.ondataavailable = event => {
          if (!session.cancelled && event.data && event.data.size > 0) session.chunks.push(event.data);
        };
        recorder.onerror = event => fail(session, 'Recording failed' + (event.error && event.error.message ? ': ' + event.error.message : '.'));
        recorder.onstop = () => transcribe(session);
        recorder.start();
        session.phase = 'recording';
        paint();
      } catch (err) {
        fail(session, microphoneMessage(err));
      }
    }
    dictationComposers.set(form, { stop: finish, render });
    button.addEventListener('click', () => {
      const session = current;
      if (!session) {
        begin();
      } else if (session.phase === 'starting') {
        finish();
      } else if (session.phase === 'recording') {
        session.phase = 'stopping';
        show(session.target, 'pending', transcribingText);
        paint();
        try {
          session.recorder.stop();
        } catch (err) {
          fail(session, 'Could not stop recording: ' + err.message);
        }
      }
    });
    render();
  }

  function stopDictation(form) {
    dictationComposers.get(form)?.stop();
  }

  function renderDictation() {
    for (const composer of dictationComposers.values()) composer.render();
  }

  function send(id, c, group = false) {
    stopDictation(c.form);
    const text = c.text.value.trim();
    if (text === '') {
      show(c.result, 'error', 'Write a message first.');
      return;
    }
    if (group && !groupLive(id)) {
      show(c.result, 'error', 'Every member is archived; this conversation is read only.');
      return;
    }
    const member = group && groupMembers(id).find(w => w.workstream === ui.aboutMember && !w.archived);
    const path = group && !member ? '/workgroups/' + id + '/conversation' : '/conversation/' + (member?.workstream || id);
    act(c.result, c.button, () => request('POST', path, { text }), (entry) => {
      c.text.value = '';
      mark([group ? groupFeed(id) : 'feed/' + id]);
      return 'Sent; the chief of staff answers in its next turn (' + entry.state + ').';
    });
  }

  function sendButton() {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS(svg.namespaceURI, 'path');
    path.setAttribute('d', 'M12 19V5m-7 7 7-7 7 7');
    svg.append(path);
    return el('button', { type: 'submit', class: 'primary icon send-icon', 'aria-label': 'Send' }, svg);
  }

  function dictationButton() {
    return byId('design-form').querySelector('[aria-label="Dictate"]').cloneNode(true);
  }

  function card(id, group = false) {
    const key = group ? 'workgroup:' + id : id;
    let c = cards.get(key);
    if (c) {
      return c;
    }
    c = {
      entries: el('div', { class: 'feed', 'data-field': 'feed' }),
      heading: el('h3', { class: 'needs-title' }, 'Waiting for you'),
      decisions: el('div', { class: 'decisions', 'data-field': 'decisions' }),
      text: el('textarea', { name: 'text', rows: '2', 'aria-label': 'Message to the chief of staff', placeholder: 'Message the chief of staff' }),
      button: sendButton(),
      result: el('p', { class: 'result', role: 'status' }),
      shown: null,
      about: el('div', { class: 'composer-about' }),
      activity: el('div', { class: 'activity-log', 'data-field': 'activity-log' }),
      activityShown: null,
      unitFilter: el('select', { name: 'unit', 'aria-label': 'Filter by unit', 'data-field': 'unit-filter' }),
    };
    c.unitFilter.addEventListener('change', () => {
      ui.unit = c.unitFilter.value;
      ui.focus = '';
      syncRoute(true);
      render();
    });
    c.activityNode = el('section', { class: 'activity', 'data-workstream': id, 'aria-label': 'Activity' },
      el('div', { class: 'activity-bar' }, el('label', {}, el('span', {}, 'Unit'), c.unitFilter)), c.activity);
    const form = el('form', { class: 'send composer', 'data-field': 'send' },
      c.about, el('div', { class: 'box' }, c.text, el('div', { class: 'actions' }, dictationButton(), c.button)), c.result);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      send(id, c, group);
    });
    // Enter sends and Shift+Enter starts a new line where there is a
    // keyboard; on a touch screen Enter starts a new line.
    c.text.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && window.matchMedia('(pointer: fine)').matches) {
        event.preventDefault();
        form.requestSubmit();
      }
    });
    c.form = form;
    c.node = el('article', { class: 'workstream', 'data-workstream': id },
      el('div', { class: 'thread' }, c.entries, c.decisions, form));
    scopeOutcome(c.result, () => ui.view === (group ? 'workgroup' : 'workstream') ? (group ? ui.workgroup : shown()?.workstream) : null, [c.button]);
    setupDictation(form, c.result, () => {
      const selected = ui.view === (group ? 'workgroup' : 'workstream') && (group || ui.tab === 'conversation')
        ? (group ? ui.workgroup : shown()?.workstream) : null;
      return selected === id ? key : null;
    });
    cards.set(key, c);
    return c;
  }

  // conversationKinds are the feed entries of the conversation with the
  // chief of staff.
  const conversationKinds = new Set(['message', 'response', 'action']);

  function role(name) {
    return name.replaceAll('_', ' ');
  }

  // sessionOutcome says how a session stands or ended, with how long it ran.
  function sessionOutcome(s) {
    const ran = s.ended_at ? duration(Math.max(0, Math.round((new Date(s.ended_at) - new Date(s.started_at)) / 1000))) : '';
    switch (s.state) {
      case 'running':
        return 'running';
      case 'done':
        return 'done in ' + ran;
      case 'waiting':
        return 'asked a question after ' + ran;
      case 'failed':
        return 'failed after ' + ran;
      default:
        return s.state;
    }
  }

  // renderConversationEntry shows one message, response, action or image of a
  // conversation. agentLabel names who answered: it defaults to the chief
  // of staff, but a session (which has no chief of staff, spec#5, spec#20)
  // passes its own agent's label instead.
  function renderConversationEntry(e, agentLabel) {
    if (e.kind === 'image') return renderImage(e, agentLabel);
    const who = { message: 'You', action: (agentLabel || 'Chief of staff') + ' acted' }[e.kind] || agentLabel || 'Chief of staff';
    return el('li', { class: 'entry', 'data-kind': e.kind, 'data-turn': e.turn, 'data-state': e.state },
      el('div', { class: 'meta meta-row' },
        el('span', { 'data-ann': 'role' }, who), ' ',
        el('span', { 'data-ann': 'when' }, when(e.at)), ' ',
        el('span', { 'data-field': 'state', 'data-ann': 'action' }, e.state),
        e.queued_reason ? ' ' : null,
        e.queued_reason ? el('span', { 'data-field': 'queued-reason' }, e.queued_reason) : null),
      el('div', { class: 'text', 'data-field': 'text' }, e.kind === 'message' ? e.text : renderMarkdown(e.text)));
  }

  // briefBody shows a recorded delivery brief: its text, its screenshots as
  // thumbnails that open the full image, and, only when it has items, the
  // "Before you ship" block, always visible and apart from the text.
  // screenshots are feed images (id, url or placeholder).
  function briefBody(text, items, screenshots) {
    return [
      el('div', { class: 'text', 'data-field': 'brief-text' }, renderMarkdown(text)),
      screenshots.length === 0 ? null : el('div', { class: 'brief-screenshots', 'data-field': 'brief-screenshots' }, ...screenshots.map((image) => image.placeholder
        ? el('p', { class: 'meta', 'data-field': 'placeholder' }, image.placeholder)
        : el('a', { class: 'image-thumbnail', href: image.url, target: '_blank', rel: 'noopener noreferrer', 'data-field': 'image-link', 'data-image': image.id, 'aria-label': 'Open full-size screenshot ' + image.id },
          el('img', { src: image.url, alt: 'Screenshot ' + image.id, 'data-field': 'image' })))),
      items.length === 0 ? null : el('section', { class: 'before-you-ship', 'data-field': 'before-you-ship', 'aria-label': 'Before you ship' },
        el('strong', { 'data-field': 'before-you-ship-label' }, 'Before you ship'),
        el('ul', { class: 'notes' }, ...items.map((item) => el('li', { 'data-field': 'before-you-ship-item' }, item)))),
    ].filter((node) => node !== null);
  }

  // renderBriefEntry shows the delivery brief the chief of staff recorded for
  // a final review as an entry of its own, so the owner can answer it in the
  // conversation right under it.
  function renderBriefEntry(e) {
    const b = e.brief;
    return el('li', { class: 'entry', 'data-kind': 'brief', 'data-review': b.review, ...(e.workstream ? { 'data-workstream': e.workstream } : {}) },
      el('div', { class: 'meta meta-row' },
        el('span', { 'data-ann': 'role' }, 'Chief of staff'), ' ',
        el('span', { 'data-ann': 'action' }, 'delivery brief for final review ' + b.review), ' ',
        el('span', { 'data-ann': 'when' }, when(e.at))),
      el('div', { class: 'card', 'data-field': 'brief' }, ...briefBody(b.text, b.before_you_ship || [], b.screenshots || [])));
  }

  // renderDeliveryBrief is the brief block of a delivery card, kept apart from
  // the rest of the card: the recorded brief, or why there is none.
  function renderDeliveryBrief(view) {
    const b = view.brief;
    if (!b) return null;
    const state = { 'data-field': 'brief', 'data-brief-state': b.state };
    if (b.state === 'recorded') {
      const shots = (b.screenshots || []).map((id) => ({ id, url: api + '/images/' + view.workstream + '/' + id }));
      return el('section', { class: 'delivery-brief', ...state }, ...briefBody(b.text || '', b.before_you_ship || [], shots));
    }
    return el('section', { class: 'delivery-brief', ...state },
      el('p', { 'data-field': 'brief-state' }, b.state === 'waiting' ? 'Waiting for the chief of staff to write the brief for this review' : 'No brief was recorded for this review'));
  }

  function renderImage(e, agentLabel) {
    const image = e.image;
    return el('li', { id: 'feed-image-' + image.id, class: 'entry', 'data-kind': 'image', 'data-image': image.id, 'data-turn': e.turn, ...(e.workstream ? { 'data-workstream': e.workstream } : {}) },
      el('div', { class: 'meta meta-row' },
        el('span', { 'data-ann': 'role' }, agentLabel || 'Chief of staff'), ' ',
        el('span', { 'data-ann': 'action' }, 'image'), ' ',
        el('span', { 'data-ann': 'when' }, when(e.at))),
      image.placeholder
        ? el('p', { class: 'meta', 'data-field': 'placeholder' }, image.placeholder)
        : el('a', { class: 'image-thumbnail', href: image.url, target: '_blank', rel: 'noopener noreferrer', 'data-field': 'image-link', 'aria-label': 'Open full-size image ' + image.id },
          el('img', { src: image.url, alt: 'Image ' + image.id, 'data-field': 'image' })));
  }

  // renderStatus shows a status the chief of staff wrote. Only the latest
  // status's attention asks for something now; marked names the latest as
  // current, where older statuses sit beside it.
  function renderStatus(e, latest, marked = false) {
    const s = e.status;
    return el('li', { class: 'entry', 'data-kind': 'status', 'data-revision': String(s.revision), 'data-latest': String(latest) },
      el('div', { class: 'meta meta-row' },
        el('span', { 'data-ann': 'role' }, 'Chief of staff'), ' ',
        el('span', { 'data-ann': 'action' }, 'status'), ' ',
        el('span', { 'data-ann': 'when' }, when(e.at)),
        latest && marked ? ' ' : null, latest && marked ? el('span', { class: 'current', 'data-field': 'current' }, 'Current') : null),
      el('div', { class: 'card' },
        s.attention ? el('div', latest ? { class: 'attention', 'data-field': 'attention' } : { class: 'meta', 'data-field': 'past-attention' }, renderMarkdown(s.attention)) : null,
        el('div', { class: 'text', 'data-field': 'note' }, renderMarkdown(s.note)),
        s.agents.length > 0 ? el('ul', { class: 'notes', 'data-field': 'status-agents' }, ...s.agents.map((a) => el('li', {}, renderMarkdown(a)))) : null));
  }

  function renderSession(e) {
    const s = e.session;
    const report = s.failure || s.summary;
    const line = el('span', { class: 'meta-row', 'data-field': 'session' },
      el('span', {}, el('strong', { 'data-ann': 'role' }, role(s.role)), s.unit ? ' on ' + s.unit : ''),
      ' ', el('span', { 'data-field': 'profile' }, s.profile),
      ' ', el('span', { 'data-field': 'outcome', 'data-ann': 'action' }, sessionOutcome(s)));
    const head = [el('span', { class: 'dot', 'aria-hidden': 'true' }), line, el('span', { class: 'meta', 'data-ann': 'when' }, when(s.started_at))];
    const node = el('li', { id: 'feed-turn-' + s.turn, class: 'entry event', 'data-kind': 'session', 'data-role': s.role, 'data-state': s.state, 'data-turn': s.turn });
    if (!report) {
      node.append(el('div', { class: 'event-line' }, ...head));
      return node;
    }
    const details = el('details', {}, el('summary', { class: 'event-line' }, ...head),
      el('div', { class: 'text', 'data-field': s.failure ? 'failure' : 'summary' }, renderMarkdown(report)));
    const key = entryKey(e);
    details.open = ui.open.has(key) || ui.focus === key;
    details.addEventListener('toggle', () => {
      if (details.open === ui.open.has(key)) return;
      if (details.open) {
        ui.open.add(key);
      } else {
        ui.open.delete(key);
        if (ui.focus === key) ui.focus = '';
      }
      syncRoute();
    });
    node.append(details);
    return node;
  }

  function renderTransition(e) {
    const t = e.transition;
    return el('li', { class: 'entry event', 'data-kind': 'transition', 'data-subject': t.subject || '', 'data-unit': t.unit || '', 'data-from': t.from, 'data-to': t.to },
      el('div', { class: 'event-line', title: t.reason },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('span', { 'data-field': 'transition' },
          el('span', { 'data-field': 'subject', 'data-ann': 'role' }, transitionSubject(t)), ': ',
          t.from ? el('span', { 'data-field': 'from' }, t.from) : null, t.from ? ' → ' : '',
          el('strong', { 'data-field': 'to', 'data-ann': 'action' }, t.to)),
        t.reason ? el('span', { class: 'reason', 'data-field': 'reason' }, t.reason) : null,
        t.departed ? el('span', { class: 'footnote decided-by', 'data-field': 'decided-by' }, 'decided by Jev') : null,
        el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))));
  }

  // recoveryStates say where a recovery card stands in Activity.
  const recoveryStates = { open: 'waiting for you', approved: 'approved', stale: 'stale, the subject moved on' };

  // renderRecoveryEntry is the quiet line a recovery card leaves in Activity:
  // the move it pins and where the card stands. The card itself, with its
  // button, is in the decisions above the composer while it is open.
  function renderRecoveryEntry(e) {
    const r = e.recovery;
    return el('li', { class: 'entry event', 'data-kind': 'recovery', 'data-state': r.state, 'data-unit': r.unit || '', 'data-from': r.from, 'data-to': r.target },
      el('div', { class: 'event-line', title: r.reason },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('span', { 'data-field': 'recovery' },
          el('span', { 'data-field': 'subject', 'data-ann': 'role' }, r.unit ? 'Unit ' + r.unit : 'Workstream'), ': recovery move ',
          el('span', { 'data-field': 'from' }, r.from), ' → ', el('strong', { 'data-field': 'to', 'data-ann': 'action' }, r.target)),
        el('span', { class: 'reason', 'data-field': 'reason' }, r.reason),
        el('span', { class: 'meta', 'data-field': 'state' }, recoveryStates[r.state] || r.state),
        el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))));
  }

  function transitionSubject(t) {
    const subject = t.subject || '';
    if (subject === 'feature' || (!subject && !t.unit)) return 'Workstream';
    if (t.unit && (!subject || subject === 'unit-' + t.unit)) return 'Unit ' + t.unit;
    if (subject.startsWith('unit-') && subject.endsWith('-checks')) return 'Unit checks ' + (t.unit || subject.slice(5, -7));
    if (subject.startsWith('unit-') && t.unit && t.unit.length > 64) return 'Unit ' + t.unit;
    const label = subject.replaceAll(/[-_]/g, ' ');
    return label.charAt(0).toUpperCase() + label.slice(1) + (t.unit ? ', unit ' + t.unit : '');
  }

  function checkState(state) {
    return state === 'incomplete' ? 'not completed' : state === 'continued' ? 'continued without results' : state;
  }

  function checkSelection(selection, running) {
    if (!selection) return null;
    if (selection.mode === 'selected') {
      return el('p', { 'data-field': 'selection' },
        selection.fallback_picks ? (running ? 'no check reached the threshold; checking the highest-scored checks, ' : 'no check reached the threshold; the highest-scored checks ran, ') : '', 'checking ',
        el('span', { 'data-field': 'selected' }, String(selection.selected)), ' of ',
        el('span', { 'data-field': 'judged' }, String(selection.judged)),
        selection.listed > selection.judged ? el('span', { 'data-field': 'listed' }, ', narrowed from ' + selection.listed + ' listed') : null);
    }
    const reasons = {
      unconfigured: 'Jev is unconfigured', declined: 'Jev declined', too_many_checks: 'too many checks',
      change_too_large: 'the change is too large', unlisted: 'checks could not be listed',
      no_checks: 'no check links were listed', diff_unreadable: 'the change could not be read',
      refused: 'Dagger refused the selected links'
    };
    const reason = reasons[selection.reason] || (selection.reason || '').replaceAll('_', ' ');
    return el('p', { 'data-field': 'selection' }, running ? 'checking every check' : 'every check ran',
      reason ? ': ' : '', el('span', { 'data-field': 'reason' }, reason),
      selection.detail ? el('span', { 'data-field': 'detail' }, ', ' + selection.detail) : null);
  }

  function renderChecks(e) {
    const unit = e.kind === 'unit_checks';
    const session = e.kind === 'session_checks';
    const run = unit ? e.unit_checks : session ? e.session_checks : e.final_checks;
    const label = unit ? 'Unit ' + run.unit + ', check run ' + run.run : session ? 'Session, check run ' + run.run + ', ' + run.kind : 'Final review ' + run.review + ', checks' + (run.attempt ? ', attempt ' + run.attempt : '');
    return el('li', { class: 'entry event check-entry', 'data-kind': e.kind, 'data-unit': run.unit || '', 'data-run': run.run || '', 'data-review': run.review || '', 'data-attempt': run.attempt || '', 'data-state': run.state },
      el('div', { class: 'event-line' },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('strong', { 'data-field': 'checks', 'data-ann': 'role' }, label),
        el('span', { 'data-field': 'state', 'data-ann': 'action' }, checkState(run.state)),
        el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))),
      run.trace_url ? el('p', {}, el('a', { href: run.trace_url, 'data-field': 'trace-url', target: '_blank', rel: 'noopener noreferrer' }, 'View Dagger Cloud trace')) : null,
      checkSelection(run.selection, run.state === 'running'),
      !unit && !session && run.selection ? el('p', { 'data-field': 'selection-description' }, run.selection.description) : null,
      run.source === 'pull_request' ? el('p', { class: 'meta-row', 'data-field': 'source' },
        el('span', {}, 'Dagger Cloud statuses of pull request #' + run.pull_request),
        el('span', {}, 'push ' + run.push),
        run.status_url ? el('a', { href: run.status_url, 'data-field': 'status-url', target: '_blank', rel: 'noopener noreferrer' }, 'status') : null) : null,
      run.counts ? el('p', { class: 'meta-row', 'data-field': 'counts' },
        el('span', { 'data-field': 'total' }, run.counts.total + ' checks'),
        el('span', { 'data-field': 'passed' }, run.counts.passed + ' passed'),
        el('span', { 'data-field': 'failed' }, run.counts.failed + ' failed'),
        el('span', { 'data-field': 'cancelled' }, run.counts.cancelled + ' cancelled')) : null,
      run.failed_checks && run.failed_checks.length ? el('ul', { class: 'notes', 'data-field': 'failed-checks' }, ...run.failed_checks.map(c => el('li', { class: 'meta-row' },
        el('span', {}, c.name),
        c.trace_id ? el('span', { 'data-field': 'trace-id' }, 'dagger trace ' + c.trace_id) : null))) : null,
      run.reruns && run.reruns.length ? el('p', { 'data-field': 'rerun-note' }, 'Failed checks rerun once, requested ' + when(run.reruns[0].requested_at)) : null,
      run.failed && run.failed.length && !(run.failed_checks && run.failed_checks.length) ? el('ul', { class: 'notes', 'data-field': 'failed' }, ...run.failed.map(link => el('li', {}, link))) : null,
      run.flaky && run.flaky.length ? el('p', { 'data-field': 'flaky-note' }, 'Flaky: failed, then passed on rerun') : null,
      run.flaky && run.flaky.length ? el('ul', { class: 'notes', 'data-field': 'flaky' }, ...run.flaky.map(link => el('li', {}, link))) : null,
      run.error ? el('p', { 'data-field': 'error' }, run.error) : null);
  }

  function renderFinalReview(e) {
    const r = e.final_review;
    return el('li', { class: 'entry event check-entry', 'data-kind': 'final_review', 'data-review': r.review, 'data-state': r.state },
      el('div', { class: 'event-line' },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('strong', { 'data-ann': 'role' }, 'Final review ' + r.review),
        el('span', { 'data-field': 'outcome', 'data-ann': 'action' }, r.state),
        el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))),
      r.reader ? el('p', { 'data-field': 'reader' }, 'Reader ' + r.reader,
        r.turn ? ' ' : null, r.turn ? sessionLink(r.turn) : null) : null,
      r.state === 'reviewed' ? el('p', { class: 'meta-row', 'data-field': 'findings' },
        el('span', { 'data-field': 'blocking' }, r.blocking + ' blocking findings'), ' ',
        el('span', { 'data-field': 'gaps' }, r.gaps + ' uncovered criteria')) : null,
      r.failure ? el('p', { 'data-field': 'failure' }, r.failure) : null);
  }

  // activityKinds are the feed entries Activity shows: the machine events
  // and every status of the chief of staff. The rest of the feed is the
  // conversation.
  const activityKinds = new Set(['status', 'session', 'transition', 'unit_checks', 'final_checks', 'session_checks', 'final_review', 'recovery']);

  // milestoneStates are the workstream states Conversation notes in a line.
  const milestoneStates = new Set(['ratified', 'building', 'assembled', 'delivered']);

  function isWorkstreamTransition(e) {
    const t = e.transition;
    return e.kind === 'transition' && (t.subject === 'feature' || (!t.subject && !t.unit));
  }

  // isMilestone reports whether Conversation shows a feed entry as a quiet
  // line: a workstream state change to ratified, building, assembled or
  // delivered, or the result of a final review. Sessions, unit transitions
  // and check runs, failed or not, are never milestones.
  function isMilestone(e) {
    return e.kind === 'final_review' || (isWorkstreamTransition(e) && milestoneStates.has(e.transition.to));
  }

  // foldable reports whether Activity may fold an entry into a run: the
  // routine events between statuses, milestones and final reviews.
  function foldable(e) {
    return activityKinds.has(e.kind) && e.kind !== 'status' && e.kind !== 'final_review' && !isMilestone(e);
  }

  // The shortest run of consecutive events that folds into one line.
  const foldMin = 3;

  // entryKey names an Activity entry in the location and the document. A
  // session is named by its turn; the rest by a short hash of the fields that
  // stay the same while an entry progresses.
  function entryKey(e) {
    if (e.kind === 'session') return 'turn-' + e.session.turn;
    const text = feedIdentity(e);
    let hash = 2166136261;
    for (let i = 0; i < text.length; i++) {
      hash = Math.imul(hash ^ text.charCodeAt(i), 16777619);
    }
    return 'e' + (hash >>> 0).toString(36);
  }

  // entryUnit is the plan unit an entry is about, or the empty string.
  function entryUnit(e) {
    return e.session?.unit || e.transition?.unit || e.unit_checks?.unit || e.recovery?.unit || '';
  }

  function activityHref(id, entry) {
    return '#workstream=' + encodeURIComponent(id) + '&tab=activity' + (entry ? '&entry=' + encodeURIComponent(entry) : '');
  }

  // activityLink is a link that opens Activity at an entry of the workstream
  // shown, in place and without a reload.
  function activityLink(attrs, entry, ...children) {
    const id = shown()?.workstream || ui.selected;
    const link = el('a', { href: activityHref(id, entry), ...attrs }, ...children);
    link.addEventListener('click', (event) => {
      if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      showActivity({ entry });
    });
    return link;
  }

  function sessionLink(turn) {
    return activityLink({ 'data-field': 'turn' }, 'turn-' + turn, 'turn ' + turn);
  }

  // renderMilestone shows a milestone as one quiet line that opens Activity
  // at the entry it comes from.
  function renderMilestone(e) {
    const key = entryKey(e);
    const base = { class: 'entry milestone', 'data-kind': 'milestone', 'data-milestone': e.kind === 'final_review' ? 'final_review' : e.transition.to, 'data-entry': key };
    if (e.kind === 'final_review') {
      const r = e.final_review;
      return el('li', { ...base, 'data-review': r.review, 'data-state': r.state },
        activityLink({ class: 'milestone-line', 'data-field': 'milestone' }, key,
          el('span', { class: 'dot', 'aria-hidden': 'true' }),
          el('strong', { 'data-ann': 'role' }, 'Final review ' + r.review),
          el('span', { 'data-ann': 'action', 'data-field': 'outcome' }, r.state),
          r.state === 'reviewed' ? el('span', { class: 'meta', 'data-field': 'findings' }, r.blocking + ' blocking findings, ' + r.gaps + ' uncovered criteria') : null,
          r.failure ? el('span', { class: 'meta', 'data-field': 'failure' }, r.failure) : null,
          el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))));
    }
    return el('li', { ...base, 'data-to': e.transition.to },
      activityLink({ class: 'milestone-line', 'data-field': 'milestone' }, key,
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('strong', { 'data-ann': 'role' }, 'Workstream'),
        el('span', { 'data-ann': 'action', 'data-field': 'to' }, e.transition.to),
        el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))));
  }

  // conversationEntries are the feed entries Conversation shows, oldest
  // first: the conversation itself, only the latest status and the
  // milestones.
  function conversationEntries(entries) {
    const latest = entries.findLastIndex((e) => e.kind === 'status');
    return entries.filter((e, i) => e.kind === 'status' ? i === latest : !activityKinds.has(e.kind) || isMilestone(e));
  }

  // renderConversation shows the workstream's conversation: what the owner
  // and the chief of staff said, the chief's latest status, and the
  // milestones, with the owner's command outcomes where they were taken.
  function renderConversation(c, id) {
    const name = 'feed/' + id;
    const view = feedView(id);
    const outcomes = ownerOutcomes.get(id) || [];
    const shown = view ? conversationEntries(view.entries) : [];
    const key = view ? JSON.stringify([shown, outcomes]) : (failures[name] ? 'failed' : 'reading');
    if (c.shown === key) {
      return;
    }
    c.shown = key;
    if (!view) {
      c.entries.replaceChildren(readState(name, 'Reading the conversation…', 'The conversation is unavailable.'),
        ...(outcomes.length ? [el('ol', {}, ...outcomes.map(renderOwnerOutcome))] : []));
      return;
    }
    if (shown.length === 0 && outcomes.length === 0) {
      const open = el('button', { type: 'button', class: 'secondary' }, 'Open activity');
      open.addEventListener('click', () => showActivity());
      c.entries.replaceChildren(el('div', { 'data-field': 'empty' }, stateBlock('empty', 'Nothing here yet.', 'Write to the chief of staff below. Sessions and check runs are in Activity.', open)));
      return;
    }
    c.entries.replaceChildren(el('ol', {}, ...withOutcomes(shown, outcomes, (e) => {
      if (e.kind === 'status') return renderStatus(e, true);
      if (isMilestone(e)) return renderMilestone(e);
      return e.kind === 'brief' ? renderBriefEntry(e) : renderConversationEntry(e);
    })));
  }

  function renderActivityEntry(e, latest) {
    let node;
    switch (e.kind) {
      case 'status':
        node = renderStatus(e, latest, true);
        break;
      case 'session':
        node = renderSession(e);
        break;
      case 'transition':
        node = renderTransition(e);
        break;
      case 'final_review':
        node = renderFinalReview(e);
        break;
      case 'recovery':
        node = renderRecoveryEntry(e);
        break;
      default:
        node = renderChecks(e);
    }
    const key = entryKey(e);
    node.dataset.entry = key;
    if (ui.focus === key) node.dataset.focus = 'true';
    return node;
  }

  // foldSummary counts what a folded run holds by kind.
  function foldSummary(run) {
    const count = (match) => run.filter(match).length;
    const sessions = count((e) => e.kind === 'session');
    const changes = count((e) => e.kind === 'transition');
    const recoveries = count((e) => e.kind === 'recovery');
    const checks = run.length - sessions - changes - recoveries;
    return [
      sessions > 0 ? sessions + (sessions === 1 ? ' session' : ' sessions') : '',
      changes > 0 ? changes + (changes === 1 ? ' state change' : ' state changes') : '',
      checks > 0 ? checks + (checks === 1 ? ' check run' : ' check runs') : '',
      recoveries > 0 ? recoveries + (recoveries === 1 ? ' recovery card' : ' recovery cards') : '',
    ].filter(Boolean).join(', ');
  }

  // renderFold shows a run of routine events as one "N events" line that
  // opens in place. It is open when the owner opened it or an entry the
  // location names is in it.
  function renderFold(run, nodes) {
    const key = 'f' + entryKey(run[0]);
    const inside = run.map(entryKey);
    const forced = inside.some((k) => k === ui.focus || ui.open.has(k));
    const details = el('details', { class: 'fold', 'data-fold': key },
      el('summary', { class: 'event-line' },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('strong', { 'data-field': 'count' }, run.length + ' events'),
        el('span', { 'data-field': 'fold-kinds' }, foldSummary(run)),
        el('span', { class: 'meta', 'data-ann': 'when' }, when(run[0].at))),
      el('ol', {}, ...nodes));
    details.open = forced || ui.open.has(key);
    details.addEventListener('toggle', () => {
      if (details.open === ui.open.has(key)) return;
      if (details.open) {
        ui.open.add(key);
      } else {
        ui.open.delete(key);
        for (const k of inside) {
          ui.open.delete(k);
          if (ui.focus === k) ui.focus = '';
        }
      }
      syncRoute();
    });
    return el('li', { class: 'fold-item', 'data-kind': 'fold', 'data-count': String(run.length) }, details);
  }

  // activityUnits lists the units the unit filter offers.
  function activityUnits(w, entries) {
    return [...new Set([...(w.units || []).map((u) => u.unit), ...entries.map(entryUnit).filter(Boolean), ...(ui.unit ? [ui.unit] : [])])].sort();
  }

  // renderActivityTab shows every event of the workstream, oldest first,
  // with a filter by unit. Routine runs fold into one line; older chief
  // statuses recede behind the latest.
  function renderActivityTab(c, w) {
    const id = w.workstream;
    const name = 'feed/' + id;
    const view = feedView(id);
    const entries = view ? view.entries.filter((e) => activityKinds.has(e.kind)) : [];
    const units = activityUnits(w, entries);
    const key = JSON.stringify([view ? entries : (failures[name] ? 'failed' : 'reading'), units, ui.unit, ui.focus]);
    if (c.activityShown === key) {
      return;
    }
    c.activityShown = key;
    c.unitFilter.replaceChildren(el('option', { value: '' }, 'All units'), ...units.map((u) => el('option', { value: u }, u)));
    c.unitFilter.value = ui.unit;
    c.unitFilter.closest('.activity-bar').hidden = units.length === 0 && ui.unit === '';
    if (!view) {
      c.activity.replaceChildren(readState(name, 'Reading the activity…', 'The activity is unavailable.'));
      return;
    }
    const latest = entries.findLastIndex((e) => e.kind === 'status');
    const listed = entries.map((e, i) => ({ e, latest: i === latest })).filter(({ e }) => !ui.unit || entryUnit(e) === ui.unit);
    if (listed.length === 0) {
      const clear = el('button', { type: 'button', class: 'secondary', 'data-field': 'clear-filter' }, 'Show all units');
      clear.addEventListener('click', () => { ui.unit = ''; syncRoute(true); render(); });
      c.activity.replaceChildren(el('div', { 'data-field': 'empty' }, ui.unit
        ? stateBlock('empty', 'No events for unit ' + ui.unit + ' yet.', 'Its sessions, state changes and check runs appear here once it runs.', clear)
        : stateBlock('empty', 'No activity yet.', 'Sessions, state changes and check runs appear here as the work runs.')));
      return;
    }
    const nodes = [];
    let run = [];
    const flush = () => {
      if (run.length >= foldMin) {
        nodes.push(renderFold(run.map((x) => x.e), run.map((x) => renderActivityEntry(x.e, x.latest))));
      } else {
        nodes.push(...run.map((x) => renderActivityEntry(x.e, x.latest)));
      }
      run = [];
    };
    for (const item of listed) {
      if (foldable(item.e)) {
        run.push(item);
      } else {
        flush();
        nodes.push(renderActivityEntry(item.e, item.latest));
      }
    }
    flush();
    c.activity.replaceChildren(el('ol', {}, ...nodes));
  }

  // focusEntry scrolls to the Activity entry the location names, once.
  function focusEntry() {
    if (!ui.scrollFocus || ui.view !== 'workstream' || ui.tab !== 'activity' || !ui.focus) return;
    const node = byId('workstream-activity').querySelector('[data-entry="' + CSS.escape(ui.focus) + '"]');
    if (!node) return;
    ui.scrollFocus = false;
    ui.toEnd = false;
    requestAnimationFrame(() => node.scrollIntoView({ block: 'center' }));
  }

  // feedView is the feed the workstream's card shows: its read feed, less the
  // conversation, statuses and chief turns the shared conversation of a
  // multi-member workgroup shows instead.
  function feedView(id) {
    const raw = views['feed/' + id];
    const multi = isMultiMember(streams().find(w => w.workstream === id));
    return raw && multi ? { ...raw, entries: raw.entries.filter(e => !conversationKinds.has(e.kind) && e.kind !== 'status' && !(e.kind === 'session' && e.session.role === 'chief_of_staff')) } : raw;
  }

  // groupEntry reports whether a workgroup feed entry is part of its
  // conversation view.
  function groupEntry(e) {
    return conversationKinds.has(e.kind) || e.kind === 'image' || e.kind === 'status' || e.kind === 'brief';
  }

  // ownerOutcomes holds, per displayed instance (a workstream ID, or
  // 'workgroup:<id>'), the accepted owner commands' outcomes the page
  // shows in that instance's feed for as long as the page lives. Each entry
  // is {text, at, known}: known lists the identities of the feed entries
  // shown when the command was accepted. The service never records them,
  // and the unread activity ignores them.
  const ownerOutcomes = new Map();

  // feedIdentity names a feed entry by the fields that stay the same while
  // it progresses, so an anchor still finds its entry after a re-read.
  function feedIdentity(e) {
    return JSON.stringify([
      e.kind, e.at, e.turn || '', e.repo || '', e.image?.id || '',
      e.session ? [e.session.agent, e.session.turn] : '',
      e.transition ? [e.transition.subject, e.transition.unit, e.transition.from, e.transition.to] : '',
      e.unit_checks ? [e.unit_checks.unit, e.unit_checks.run] : '',
      e.session_checks ? [e.session_checks.run, e.session_checks.kind] : '',
      e.final_checks ? [e.final_checks.review, e.final_checks.attempt] : '',
      e.final_review ? e.final_review.review : '',
      e.brief ? e.brief.review : '',
      e.recovery ? e.recovery.occurrence : '']);
  }

  // shownEntries lists the feed entries the instance's feed shows now, or
  // null while that feed is not read.
  function shownEntries(key) {
    if (key.startsWith('workgroup:')) {
      return views[groupFeed(key.slice('workgroup:'.length))]?.entries.filter(groupEntry) || null;
    }
    return feedView(key)?.entries || null;
  }

  // acceptOutcome adds the accepted command's outcome to the feed of the
  // instance the command was taken on and clears that instance's result
  // line. An action taken where no instance is displayed keeps its text in
  // the result line.
  function acceptOutcome(target, text) {
    if (!target.scope || target.key === null) {
      show(target, 'ok', text);
      return;
    }
    const known = (shownEntries(target.key) || []).map(feedIdentity);
    const held = ownerOutcomes.get(target.key) || [];
    ownerOutcomes.set(target.key, [...held, { text, at: new Date().toISOString(), known }]);
    target.scope.entries.delete(target.key);
    paintOutcome(target.scope);
    render();
  }

  // withOutcomes renders the feed entries and puts each page-held outcome
  // after the entries the feed showed when the command was accepted. An
  // entry the outcome's command did not see, wherever the read feed orders
  // it, renders after that outcome, and the entries of each stretch keep the
  // feed's order.
  function withOutcomes(entries, outcomes, renderEntry) {
    const seen = outcomes.map((o) => new Set(o.known));
    const stretch = entries.map((e) => {
      const id = feedIdentity(e);
      const first = seen.findIndex((s) => s.has(id));
      return first < 0 ? outcomes.length : first;
    });
    const nodes = [];
    for (let n = 0; n <= outcomes.length; n++) {
      entries.forEach((e, i) => { if (stretch[i] === n) nodes.push(renderEntry(e, i)); });
      if (n < outcomes.length) nodes.push(renderOwnerOutcome(outcomes[n]));
    }
    return nodes;
  }

  function renderOwnerOutcome(o) {
    return el('li', { class: 'entry event', 'data-kind': 'outcome', 'data-outcome': 'ok' },
      el('div', { class: 'event-line' },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('span', { 'data-field': 'outcome' },
          el('strong', { 'data-ann': 'role' }, 'Your command'), ': ',
          el('span', { 'data-field': 'text', 'data-ann': 'action' }, o.text)),
        el('span', { class: 'meta', 'data-ann': 'when' }, when(o.at))));
  }

  // renderDecisions shows what waits for the owner on the workstream above
  // its message field, with the outcome of the last answer.
  function renderDecisions(c, id) {
    const nodes = [...inboxOf(id).map(renderDecision), ...proposalsOf(id).map(renderProposal)];
    place(c.decisions, [...(nodes.length > 0 ? [c.heading] : []), ...nodes, inboxResult]);
  }

  // tubeTone groups the unit states into the status the nest tube paints:
  // idle units are neutral, units a mason, check or reviewer is working on
  // are live, a unit held back waits, and a unit past review is done. A
  // contested unit is marked apart from its colour.
  const tubeTones = {
    planned: 'idle',
    ready: 'ready',
    implementing: 'live',
    waiting: 'waiting',
    checking: 'live',
    reviewing: 'live',
    approved: 'done',
    contested: 'contested',
    merged: 'done',
  };

  // nestTube draws one cell per unit, in the order given. A cell with a
  // name is a button that opens Activity on that unit; a cell without one,
  // as in a workgroup member's tally, only shows its state. Hovering,
  // focusing or pressing a cell names it in the readout, and each cell
  // carries the unit and its state as its accessible name.
  function nestTube(cells, { linked }) {
    const total = cells.length + (cells.length === 1 ? ' unit' : ' units');
    const name = el('span', { class: 'tube-name' }, total);
    const state = el('span', { class: 'tube-state' });
    const readout = el('span', { class: 'tube-readout', 'data-field': 'unit-name', 'aria-hidden': 'true' }, name, state);
    const show = (cell) => {
      name.textContent = cell ? cell.name || cell.state : total;
      state.textContent = cell && cell.name ? cell.state : '';
      readout.dataset.named = String(cell !== null && cell.name !== '');
    };
    const tube = el('div', { class: 'nest-tube', 'data-field': 'units', role: 'group', 'aria-label': 'Units, ' + total },
      ...cells.map((cell) => {
        const label = cell.name ? 'Unit ' + cell.name + ': ' + cell.state : cell.state;
        const body = el('span', { class: 'cell-body', 'aria-hidden': 'true' });
        const attrs = { class: 'cell', 'data-unit-state': cell.state, 'data-tone': tubeTones[cell.state] || 'idle', 'aria-label': label };
        if (!linked || !cell.name) {
          return el('span', { ...attrs, role: 'img' }, body);
        }
        const button = el('button', { ...attrs, type: 'button', 'data-cell': cell.name }, body);
        button.addEventListener('mouseenter', () => show(cell));
        button.addEventListener('focus', () => show(cell));
        button.addEventListener('pointerdown', () => show(cell));
        button.addEventListener('mouseleave', () => show(null));
        button.addEventListener('blur', () => show(null));
        button.addEventListener('click', () => showActivity({ unit: cell.name }));
        return button;
      }), readout);
    return tube;
  }

  // memberTube draws a workgroup member's units, which the read gives as a
  // count per state, as a nest tube of unnamed cells.
  function memberTube(counts) {
    const cells = Object.entries(counts || {})
      .sort(([a], [b]) => unitOrder.indexOf(a) - unitOrder.indexOf(b))
      .flatMap(([state, count]) => Array.from({ length: count }, () => ({ name: '', state })));
    return cells.length > 0 ? nestTube(cells, { linked: false }) : el('div', { class: 'nest-tube', 'data-field': 'units' });
  }

  // renderUnitCounts draws the workstream's units as a nest tube.
  function renderUnitCounts(units) {
    if (!units || units.length === 0) {
      return null;
    }
    return nestTube(units.map((u) => ({ name: u.unit, state: u.state })), { linked: true });
  }

  // copyText puts text on the clipboard, falling back to a selected
  // textarea where the clipboard API is unavailable. It resolves to whether
  // the text was copied.
  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) {
      const area = el('textarea', { 'aria-hidden': 'true', tabindex: '-1', readonly: '' });
      area.value = text;
      area.className = 'copy-scratch';
      document.body.append(area);
      area.select();
      let copied = false;
      try {
        copied = document.execCommand('copy');
      } catch (_) {
        copied = false;
      }
      area.remove();
      return copied;
    }
  }

  // renderIDCopy shows the workstream ID in the mono face beside a control
  // that copies it.
  function renderIDCopy(id) {
    const button = el('button', { type: 'button', class: 'quiet copy-id', 'data-copy-id': id, 'aria-label': 'Copy workstream ID' }, 'Copy');
    let reset = 0;
    button.addEventListener('click', async () => {
      const copied = await copyText(id);
      button.textContent = copied ? 'Copied' : 'Copy failed';
      button.setAttribute('aria-label', (copied ? 'Copied' : 'Copy failed for') + ' workstream ID');
      button.dataset.copied = String(copied);
      clearTimeout(reset);
      reset = setTimeout(() => {
        button.textContent = 'Copy';
        button.setAttribute('aria-label', 'Copy workstream ID');
        delete button.dataset.copied;
      }, 2000);
    });
    return el('span', { class: 'id-copy' }, el('code', { class: 'workstream-id', 'data-field': 'workstream-id' }, id), button);
  }

  // shownHead is the markup the header was last drawn from, so a read that
  // changes nothing in the header leaves its elements, and a press on one,
  // alone.
  let shownHead = '';

  function renderHead(w) {
    const head = byId('workstream-head');
    const status = w.status;
    head.dataset.workstream = w.workstream;
    // The menu offers what the workstream's state allows: asking for a drift
    // rebase of a building, assembled or delivered branch, recording your own merge of an
    // assembled branch, abandoning work in progress, archiving finished work
    // with permanent trace cleanup.
    const offered = {
      rebase: !w.archived && ['building', 'assembled', 'delivered'].includes(w.state),
      merged: w.state === 'assembled',
      abandon: !terminal(w),
      'abandon-archive': !terminal(w),
      archive: terminal(w) && !w.archived,
    };
    for (const button of document.querySelectorAll('[data-workstream-action]')) {
      const action = button.dataset.workstreamAction;
      button.hidden = action in offered && !offered[action];
    }
    const pill = w.state ? el('span', { class: 'state-pill', 'data-field': 'state', 'data-glyph': stateGlyph(w) },
      el('span', { class: 'glyph', 'aria-hidden': 'true', 'data-glyph': stateGlyph(w) }), w.state) : null;
    const nodes = [
      el('h2', { 'data-field': 'goal' }, isMultiMember(w) ? repoLabel(w.project) : status ? status.goal : 'No status yet'),
      el('div', { class: 'head-meta' }, pill,
        w.archived ? el('span', { class: 'tag', 'data-field': 'archived' }, w.archived_by === 'chief_of_staff' ? 'archived by chief of staff' : 'archived') : null,
        el('span', { class: 'head-project', 'data-field': 'project' }, repoLabel(w.project)),
        renderIDCopy(w.workstream)),
      renderUnitCounts(w.units),
      el('details', { class: 'head-details' }, el('summary', {}, 'Details'),
        el('p', { class: 'meta', 'data-field': 'workspaces' }, w.workspaces + ' workspaces')),
      isMultiMember(w) ? renderChiefLine(w) : null].filter((node) => node !== null);
    const markup = nodes.map((node) => node.outerHTML).join('');
    if (markup === shownHead && head.childElementCount > 0) return;
    shownHead = markup;
    const open = head.querySelector('.head-details')?.open === true;
    head.replaceChildren(...nodes);
    if (open) head.querySelector('.head-details').open = true;
  }

  function renderChiefLine(w) {
    const ask = el('a', { href: '#workgroup=' + encodeURIComponent(w.workgroup.id), 'data-ask-chief': w.workstream }, 'Ask the chief about this repo');
    ask.addEventListener('click', event => {
      event.preventDefault();
      selectWorkgroup(w.workgroup.id, w.workstream);
      const c = card(w.workgroup.id, true);
      c.text.focus();
      c.form.scrollIntoView({ block: 'nearest' });
    });
    const status = views[groupFeed(w.workgroup.id)]?.entries.findLast(e => e.kind === 'status')?.status || w.status;
    return el('div', { class: 'member-chief-line', 'data-field': 'chief-status' },
      el('span', {}, 'Chief: ', status?.note || status?.attention || 'No status yet'), ask);
  }

  function renderWorkstream(w) {
    const c = card(w.workstream);
    if (isMultiMember(w)) {
      c.form.remove();
      c.text.value = '';
    } else if (!c.form.isConnected) {
      c.node.querySelector('.thread').append(c.form);
    }
    renderConversation(c, w.workstream);
    if (ui.view !== 'workgroup') renderDecisions(c, w.workstream);
    return c.node;
  }

  function renderWorkstreamView() {
    const box = byId('workstreams');
    const ids = new Set(streams().map((w) => w.workstream));
    for (const id of cards.keys()) {
      if (!ids.has(id) && !workgroups().some(g => id === 'workgroup:' + g.id)) {
        const form = cards.get(id).form;
        stopDictation(form);
        dictationComposers.delete(form);
        cards.delete(id);
      }
    }
    const w = views.status ? shown() : null;
    byId('nothing').hidden = w !== null || !views.status;
    byId('nothing').querySelector('p').textContent = streams().length === 0 ? 'No workstreams yet.' : 'Every workstream is archived.';
    byId('workstream-top').hidden = w === null;
    for (const panel of document.querySelectorAll('[data-panel]')) {
      panel.hidden = w === null || panel.dataset.panel !== ui.tab;
    }
    for (const tab of document.querySelectorAll('[data-tab]')) {
      tab.setAttribute('aria-selected', String(tab.dataset.tab === ui.tab));
    }
    if (w === null) {
      delete byId('workstream-head').dataset.workstream;
      byId('workstream-head').replaceChildren();
      place(box, []);
      box.after(inboxResult);
      return;
    }
    renderHead(w);
    place(box, [renderWorkstream(w)]);
    const c = card(w.workstream);
    if (ui.tab === 'activity') renderActivityTab(c, w);
    place(byId('workstream-activity'), [c.activityNode]);
  }

  // sessionEchoes holds, per session, the owner's own sent messages by turn
  // ID, keyed the way the POST response names them. The session's feed
  // (read through the ordinary feed endpoint) never carries the owner's
  // prompt text back, only the agent's reply once a turn finishes, so the
  // page keeps its own echo to show the sent message at once and keep
  // showing it once the reply arrives.
  const sessionEchoes = new Map();

  function sessionEchoesFor(id) {
    let m = sessionEchoes.get(id);
    if (!m) {
      m = new Map();
      sessionEchoes.set(id, m);
    }
    return m;
  }

  // sessionTurnState maps a session's feed entry state to the conversation
  // state renderConversationEntry shows.
  function sessionTurnState(state) {
    switch (state) {
      case 'running':
        return 'running';
      case 'failed':
        return 'failed';
      case 'interrupted':
        return 'stopped';
      default:
        return 'done';
    }
  }

  // sessionConversationEntries builds the session's chat, oldest first, the
  // same shape the chief-of-staff conversation uses: the owner's message,
  // from this page's own echo, and the agent's reply, from the session's
  // feed, once the turn that answers it finishes. A turn just sent, not yet
  // in the feed, still shows from its echo alone.
  function sessionConversationEntries(id) {
    const echoes = sessionEchoesFor(id);
    const feed = views['feed/' + id];
    const out = [];
    const seen = new Set();
    if (feed) {
      for (const e of feed.entries) {
        if (e.kind === 'session_checks' || e.kind === 'image') {
          out.push(e);
          continue;
        }
        if (e.kind !== 'session') {
          continue;
        }
        const s = e.session;
        seen.add(s.turn);
        const echo = echoes.get(s.turn);
        const state = sessionTurnState(s.state);
        out.push({ turn: s.turn, kind: 'message', text: echo ? echo.text : '', at: e.at, state });
        const reply = s.failure || s.summary;
        if (state !== 'running' && reply) {
          out.push({ turn: s.turn, kind: 'response', text: reply, at: s.ended_at || e.at, state });
        }
      }
    }
    for (const [turn, echo] of echoes) {
      if (!seen.has(turn)) {
        out.push(echo);
      }
    }
    return out;
  }

  let sessionFeedShown = null;

  function renderSessionFeed(id) {
    const entries = sessionConversationEntries(id);
    const key = id + ':' + JSON.stringify(entries);
    if (sessionFeedShown === key) {
      return;
    }
    sessionFeedShown = key;
    const box = byId('session-feed');
    if (entries.length === 0) {
      box.replaceChildren(stateBlock('empty', 'Nothing has happened yet.', 'Message the agent below; it works in the session\'s own worktree and its answers appear here.'));
    } else {
      box.replaceChildren(el('ol', {}, ...entries.map((e) => e.kind === 'session_checks' ? renderChecks(e) : renderConversationEntry(e, 'Agent'))));
    }
  }

  // sendSession posts the owner's message to the session's agent, the same
  // way the chief-of-staff composer's send does: the sent message shows at
  // once, from the echo this records, and the reply appears once the
  // session's feed is read again, without a reload. A busy refusal, or one
  // naming a session that has ended, shows in the composer's result.
  function sendSession(id) {
    const form = byId('session-form');
    stopDictation(form);
    const result = byId('session-result');
    const text = form.elements.text.value.trim();
    if (text === '') {
      show(result, 'error', 'Write a message first.');
      return;
    }
    act(result, form.querySelector('[type=submit]'), () => request('POST', '/sessions/' + id + '/message', { text }), (entry) => {
      if (ui.selected === id) form.elements.text.value = '';
      sessionEchoesFor(id).set(entry.turn, entry);
      mark(['status', 'feed/' + id]);
      if (ui.selected === id) {
        renderSessionFeed(id);
        snapFeed();
      }
      return 'Sent; the agent answers in its next turn (' + entry.state + ').';
    });
  }

  function stopSession(id) {
    act(byId('session-control-result'), byId('session-stop'), () => request('POST', '/sessions/' + id + '/stop'), () => {
      mark(['status']);
      return 'Stopped; the worktree keeps what the turn already changed.';
    });
  }

  // sessionCheckResults keeps each session's latest check result, by
  // workstream, for the check button's own result area; it holds only what
  // this page itself has run, since the API has no endpoint to read a
  // session's past check runs back.
  const sessionCheckResults = new Map();

  function renderSessionCheckResult(id) {
    const run = sessionCheckResults.get(id);
    byId('session-check-result').textContent = run
      ? 'Latest check, run ' + run.run + ' (' + run.kind + '): ' + run.status + ' (exit ' + run.exit_code + ') on ' + run.commit.slice(0, 10) + '.'
      : 'No check has run yet.';
  }

  function checkSession(id) {
    act(byId('session-control-result'), byId('session-check'), () => request('POST', '/sessions/' + id + '/check'), (run) => {
      sessionCheckResults.set(id, run);
      if (ui.selected === id) renderSessionCheckResult(id);
      return 'Check run ' + run.run + ' (' + run.kind + '): ' + run.status + '.';
    });
  }

  function pushSession(id) {
    act(byId('session-control-result'), byId('session-push'), () => request('POST', '/sessions/' + id + '/push'),
      (out) => 'Pushed ' + out.commit.slice(0, 10) + ' to ' + out.remote + '/' + out.branch + '.');
  }

  function endSession(id) {
    act(byId('session-control-result'), byId('session-end'), () => request('POST', '/sessions/' + id + '/end'), () => {
      mark(['status', 'sessions/' + id + '/changes']);
      return 'Session ended; its branch, commits and thread stay readable.';
    });
  }

  // destroyConversation asks the owner to confirm, then destroys the session.
  function destroyConversation(id, design) {
    const button = byId(design ? 'design-destroy' : 'session-destroy');
    confirmDanger({
      title: 'Destroy this ' + (design ? 'design session' : 'session') + '?',
      consequence: ['The session, its local branch (including unpushed commits) and all its history will be removed permanently. This cannot be undone.', 'Pushed branches stay on the remote.'],
      accept: design ? 'Destroy design session' : 'Destroy session',
      opener: button,
      run: () => runDestroy(id, design),
    });
  }

  function runDestroy(id, design) {
    const button = byId(design ? 'design-destroy' : 'session-destroy');
    const result = byId(design ? 'design-destroy-result' : 'session-control-result');
    act(result, button, () => request('POST', '/' + (design ? 'design' : 'sessions') + '/' + id + '/destroy'), () => {
      designIds.delete(id);
      sessionIds.delete(id);
      sessionEchoes.delete(id);
      sessionCheckResults.delete(id);
      sessionCommitRetries.delete(id);
      for (const name of ['feed/' + id, 'design/' + id, 'design/' + id + '/conversation', 'sessions/' + id + '/changes']) {
        delete views[name];
        delete failures[name];
        dirty.delete(name);
      }
      if (views.status) views.status.workstreams = streams().filter(w => w.workstream !== id);
      if (ui.selected === id) {
        ui.selected = null;
        store('selected', null);
        history.replaceState(null, '', location.pathname + location.search);
        if (ui.view === 'design' || ui.view === 'session') openView('project-pane');
      }
      render();
      mark(['status']);
      return 'Session destroyed permanently.';
    });
  }

  // sessionCommitRetries keeps each session's last attempted commit message
  // with the request key it was sent under, the way handinRetry does for
  // hand-in, so retrying the same message after a failure or a restart
  // produces exactly one commit.
  const sessionCommitRetries = new Map();

  function randomRequestKey() {
    return 'web-' + Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('');
  }

  function commitSession(id) {
    const form = byId('session-commit-form');
    const result = form.querySelector('.result');
    const message = form.elements.message.value.trim();
    if (message === '') {
      show(result, 'error', 'Write a commit message first.');
      return;
    }
    let retry = sessionCommitRetries.get(id);
    if (!retry || retry.message !== message) {
      retry = { message, key: randomRequestKey() };
      sessionCommitRetries.set(id, retry);
    }
    act(result, form.querySelector('[type=submit]'), () => request('POST', '/sessions/' + id + '/commit', { message, request_key: retry.key }), (out) => {
      sessionCommitRetries.delete(id);
      if (ui.selected === id) form.reset();
      mark(['sessions/' + id + '/changes']);
      return 'Committed ' + out.commit.slice(0, 10) + '.';
    });
  }

  function renderSessionChanges(id) {
    const view = views['sessions/' + id + '/changes'];
    const name = 'sessions/' + id + '/changes';
    const diff = byId('session-diff');
    diff.textContent = view ? (view.changes || 'No uncommitted changes.') : failures[name] ? 'The changes could not be read.' : 'Reading changes…';
    diff.dataset.state = view ? 'ready' : failures[name] ? 'error' : 'loading';
    const list = byId('session-commits');
    if (!view && failures[name]) {
      list.replaceChildren(el('li', {}, retryButton([name])));
    } else if (!view || view.commits.length === 0) {
      list.replaceChildren(el('li', { class: 'meta' }, view ? 'No commits yet. Commit the changes above to record them.' : 'No commits yet.'));
    } else {
      list.replaceChildren(...view.commits.map((c) => el('li', { 'data-commit': c.commit },
        el('code', {}, c.commit.slice(0, 10)), ' ', c.message.split('\n')[0])));
    }
  }

  // renderSessionView shows the session the owner selected: its profile with
  // no control to change it, its chat, a stop control, its changes, a commit
  // form, a check button with its latest result, push, end and destroy. It shows no
  // spec, plan, unit, review or delivery panel; sessions have none of those.
  function renderSessionView() {
    const w = shownSession();
    if (!w) {
      byId('session-title').textContent = 'Session';
      byId('session-meta').replaceChildren();
      byId('session-feed').replaceChildren(stateBlock('empty', 'No session selected.', 'Open an exploratory session to chat with an agent in its own worktree.', openButton('Open an exploratory session', 'session-add', true)));
      sessionFeedShown = null;
      block(byId('session-destroy'), true);
      byId('session-diff').textContent = '';
      byId('session-commits').replaceChildren();
      return;
    }
    byId('session-title').textContent = w.title || w.workstream;
    byId('session-meta').replaceChildren(
      el('span', { 'data-field': 'project' }, repoLabel(w.project)), ' ', el('code', {}, w.workstream), ' profile ',
      el('span', { class: 'tag', 'data-field': 'profile' }, w.profile), ' ',
      el('span', { class: 'tag', 'data-field': 'state' }, w.state));
    renderSessionFeed(w.workstream);
    renderSessionChanges(w.workstream);
    renderSessionCheckResult(w.workstream);
    const ended = w.state === 'ended';
    block(byId('session-destroy'), false);
    const form = byId('session-form');
    form.elements.text.disabled = ended;
    block(form.querySelector('[type=submit]'), ended);
    block(byId('session-stop'), ended || w.state !== 'working');
    block(byId('session-check'), ended);
    block(byId('session-push'), ended);
    block(byId('session-end'), ended);
    byId('session-commit-form').elements.message.disabled = ended;
    block(byId('session-commit-form').querySelector('[type=submit]'), ended);
  }

  // sessionProfiles lists the project's selectable profiles, the same set
  // the service runs mason turns on: the mason role's configured profile,
  // then its fallback chain, from the loaded configuration the page already
  // reads.
  function sessionProfiles() {
    const cfg = views.config ? views.config.effective : null;
    const mason = cfg && cfg.roles ? cfg.roles.mason : null;
    if (!mason || !cfg.profiles) {
      return [];
    }
    const seen = new Set();
    const out = [];
    for (let name = mason.profile; name && cfg.profiles[name] && !seen.has(name); name = cfg.profiles[name].fallback) {
      seen.add(name);
      out.push(name);
    }
    return out;
  }

  function renderSessionAddForm() {
    const form = byId('session-add-form');
    setOptions(form.elements.profile, [['', 'Choose a profile'], ...sessionProfiles().map((p) => [p, p])]);
  }

  // shownDesign is the design session the design view shows: the selected
  // workstream, when the status lists it as one. Like shownSession, it never
  // falls back to another design session.
  function shownDesign() {
    if (!isDesignId(ui.selected)) {
      return null;
    }
    return streams().find((w) => !w.archived && w.workstream === ui.selected) || designIds.get(ui.selected) || null;
  }

  // designIds names every design session this page has opened or seen
  // listed, so select can route to the design view the moment one opens,
  // before the next status read lists it.
  const designIds = new Map();
  const startedDesignIds = new Set();

  function isDesignId(id) {
    if (startedDesignIds.has(id)) {
      return false;
    }
    // Opening a design session produces several trace commits. A status
    // read already in flight may list its workstream before the design
    // marker exists. The successful open response takes precedence over
    // that snapshot until the owner explicitly starts the workgroup.
    if (designIds.has(id)) {
      return true;
    }
    const w = views.status ? views.status.workstreams.find((x) => x.workstream === id) : undefined;
    return !!(w && w.kind === 'design');
  }

  // designConversationEntries reads the design session's conversation from
  // its own endpoint, the same entry shape the chief-of-staff conversation
  // uses. Unlike a session, a design session carries no feed event of its
  // own, so a sent message waits for this view to read again, the same way
  // the ordinary chief-of-staff composer's send does.
  function designConversationEntries(id) {
    const view = views['design/' + id + '/conversation'];
    return view ? view.entries : [];
  }

  let designFeedShown = null;

  function renderDesignFeed(id) {
    const entries = designConversationEntries(id);
    const key = id + ':' + JSON.stringify(entries);
    if (designFeedShown === key) {
      return;
    }
    const box = byId('design-feed');
    // Snapshot the current DOM so a newly arrived reply keeps disclosures
    // open without depending on the asynchronous details toggle event.
    const opened = new Set();
    if (box.dataset.conversation === id) {
      for (const details of box.querySelectorAll('details[open][data-assumption]')) {
        opened.add(details.dataset.assumption);
      }
    }
    box.dataset.conversation = id;
    designFeedShown = key;
    if (entries.length === 0) {
      box.replaceChildren(stateBlock('empty', 'Nothing has happened yet.', 'Describe the feature below; the design agent answers and drafts a brief.'));
    } else {
      box.replaceChildren(el('ol', {}, ...entries.map((e, index) => {
        const entry = renderConversationEntry(e, 'Design agent');
        if (e.kind === 'response') {
          const text = entry.querySelector('[data-field="text"]');
          const pattern = /<details>\s*<summary>Assumptions<\/summary>([\s\S]*?)<\/details>/gi;
          let start = 0;
          let disclosure = 0;
          let match;
          text.replaceChildren();
          while ((match = pattern.exec(e.text)) !== null) {
            const identity = index + '/' + disclosure++;
            const details = el('details', { 'data-assumption': identity },
              el('summary', {}, 'Assumptions (reply to override)'), renderMarkdown(match[1].trim()));
            details.open = opened.has(identity);
            text.append(renderMarkdown(e.text.slice(start, match.index)), details);
            start = pattern.lastIndex;
          }
          text.append(renderMarkdown(e.text.slice(start)));
        }
        return entry;
      })));
    }
  }

  // sendDesign posts the owner's message to the design session's agent. The
  // sent message and the agent's reply both appear once this view's next
  // read of the conversation shows them.
  function sendDesign(id) {
    const form = byId('design-form');
    stopDictation(form);
    const result = byId('design-result');
    const text = form.elements.text.value.trim();
    if (text === '') {
      show(result, 'error', 'Write a message first.');
      return;
    }
    act(result, form.querySelector('[type=submit]'), () => request('POST', '/design/' + id + '/message', { text }), (entry) => {
      if (ui.selected === id) form.elements.text.value = '';
      mark(['design/' + id + '/conversation']);
      return 'Sent; the design agent answers in its next turn (' + entry.state + ').';
    });
  }

  // startDesign is the one owner action that starts a workgroup from a
  // design session's drafted brief; the same conversation continues as the
  // workgroup's chief-of-staff thread.
  function startDesign(id) {
    const revision = Number(byId('design-brief').dataset.revision);
    act(byId('design-start-result'), byId('design-start'), () => request('POST', '/design/' + id + '/start', { brief_revision: revision }), (out) => {
      designIds.delete(id);
      startedDesignIds.add(id);
      ui.tab = 'conversation';
      select(out.workstream);
      mark(['status', 'feed/' + out.workstream]);
      return 'Started a workgroup from this brief; workstream ' + out.workstream + ' is now ' + out.state + '.';
    }, (err) => {
      if (err.code === 'conflict') mark(['design/' + id]);
    });
  }

  // Keep the rendered revision with the panel so Start pins exactly what
  // the owner saw, even while another read is in flight.
  function renderDesignBrief(w) {
    const panel = byId('design-brief');
    const box = byId('design-brief-content');
    const path = w && 'design/' + w.workstream;
    const design = path && views[path]?.design;
    block(byId('design-start'), !design || !!failures[path]);
    if (!design || failures[path]) {
      delete panel.dataset.revision;
      delete panel.dataset.shown;
      box.replaceChildren(!w
        ? stateBlock('empty', 'No design session selected.', 'Open a design session to draft a brief with the design agent.', openButton('Open a design session', 'design-add', true))
        : failures[path]
          ? stateBlock('error', 'The brief could not be read.', 'Start stays off until the brief is read again.', retryButton([path]))
          : stateBlock('loading', 'Reading the brief…'));
      return;
    }
    const key = JSON.stringify([design, design.projects.map(projectName), projectName(design.project)]);
    if (panel.dataset.shown === key) return;
    panel.dataset.shown = key;
    panel.dataset.revision = String(design.brief_revision);
    box.replaceChildren(
      el('p', { 'data-field': 'revision' }, 'Brief revision: ' + design.brief_revision),
      el('p', { class: 'meta', 'data-field': 'home' }, 'Home project: ' + projectName(design.project)),
      el('ul', { 'data-field': 'projects' }, ...design.projects.map(id =>
        el('li', { 'data-project': id }, projectName(id) + (id === design.project ? ' (home)' : '')))),
      ...(design.new_project ? [renderDraftedNewProject(design.new_project)] : []),
      design.brief_revision > 0
        ? el('details', {}, el('summary', {}, 'Brief text'), renderMarkdown(design.brief))
        : stateBlock('empty', 'The agent has not drafted a brief yet.', 'Describe the feature to the design agent; the brief appears here as it takes shape.'));
  }

  // renderDraftedNewProject shows the project the brief asks Start to
  // create, with the visibility its repository will have.
  function renderDraftedNewProject(np) {
    return el('div', { 'data-field': 'new-project' },
      el('h4', {}, 'New project: ' + np.name),
      el('p', { 'data-field': 'new-project-repo' }, 'Repository: ' + np.repo),
      el('p', { 'data-field': 'new-project-visibility' }, 'Visibility: ' + np.visibility),
      np.description ? el('p', { 'data-field': 'new-project-description' }, 'Description: ' + np.description) : null,
      el('details', {}, el('summary', {}, 'First charter'), renderMarkdown(np.charter)));
  }

  function renderDesignView() {
    const w = shownDesign();
    renderDesignBrief(w);
    if (!w) {
      byId('design-title').textContent = 'Design session';
      byId('design-meta').replaceChildren();
      byId('design-feed').replaceChildren(stateBlock('empty', 'No design session selected.', 'Open a design session to talk a feature through before handing it in.', openButton('Open a design session', 'design-add', true)));
      designFeedShown = null;
      block(byId('design-destroy'), true);
      return;
    }
    // The design read is the freshest title once it has loaded; until then the
    // status entry's title stands. An untitled session shows its ID.
    const read = views['design/' + w.workstream]?.design;
    byId('design-title').textContent = (read ? read.title : w.title) || w.workstream;
    block(byId('design-destroy'), false);
    byId('design-meta').replaceChildren(el('span', { 'data-field': 'project' }, projectName(w.project)), ' ', el('code', {}, w.workstream));
    renderDesignFeed(w.workstream);
  }

  // renderHeader shows the slots in use per role, the pauses in force and
  // whether a reload has something to apply.
  function renderHeader() {
    const tailnet = views.status?.tailnet;
    const https = byId('tailnet-https-status');
    https.hidden = !tailnet;
    https.textContent = tailnet ? 'Tailnet HTTPS: ' + (tailnet.https_state === 'up'
      ? tailnet.https_address || tailnet.https_state : tailnet.https_state + (tailnet.https_reason ? ' — ' + tailnet.https_reason : '')) : '';
    const capacity = views.status ? views.status.capacity : null;
    const meters = byId('meters');
    if (!capacity) {
      meters.replaceChildren();
      byId('meter-total').textContent = '';
      byId('capacity-button').dataset.full = 'false';
    } else {
      const used = capacity.roles.filter(boundedPool).reduce((n, r) => n + r.used, 0);
      const limit = capacity.roles.filter(boundedPool).reduce((n, r) => n + r.limit, 0);
      const local = capacity.roles.filter((r) => !boundedPool(r)).reduce((n, r) => n + r.used, 0);
      meters.replaceChildren(...capacity.roles.map((r) => {
        const fill = el('span', { class: 'fill' });
        fill.style.height = (boundedPool(r) && r.limit > 0 ? Math.min(100, Math.round(100 * r.used / r.limit)) : r.used > 0 ? 100 : 0) + '%';
        return el('span', { class: 'meter', 'data-meter': r.role, 'data-full': String(boundedPool(r) && r.used >= r.limit), 'data-waiting': String(r.waiting.length > 0),
          title: r.role + ': ' + poolUse(r) + (r.waiting.length > 0 ? ', ' + r.waiting.length + ' waiting' : '') }, fill);
      }));
      byId('meter-total').textContent = used + '/' + limit;
      byId('capacity-button').dataset.full = String(capacity.roles.some((r) => boundedPool(r) && r.used >= r.limit));
      byId('capacity-button').title = used + ' of ' + limit + ' slots in use' + (local > 0 ? '; ' + local + ' architect sessions' : '');
    }
    const pauses = views.runtime ? views.runtime.effective.pauses || [] : [];
    const button = byId('pause-button');
    button.dataset.paused = String(pauses.length > 0);
    button.title = pauses.length === 0 ? 'Nothing is paused' : pauses.length + ' paused';
    byId('pause-count').textContent = pauses.length > 0 ? String(pauses.length) : '';
    const activity = views.activity ? views.activity.activity : [];
    const blocked = activity.filter(activityBlocked).length;
    const activityButton = byId('activity-button');
    activityButton.dataset.active = String(activity.length > 0);
    activityButton.dataset.blocked = String(blocked > 0);
    activityButton.title = activity.length === 0 ? 'Nothing is running' : activity.length + ' running' + (blocked > 0 ? ', ' + blocked + ' blocked or failed' : '');
    byId('activity-count').textContent = activity.length > 0 ? String(activity.length) : '';
    const lit = views.config ? reloadable(views.config) : false;
    byId('settings-button').dataset.lit = String(lit);
    byId('config-lit').hidden = !lit;
  }

  function renderViews() {
    for (const view of document.querySelectorAll('#main > .view')) {
      view.hidden = view.dataset.view !== ui.view;
    }
    for (const section of document.querySelectorAll('.settings-section')) {
      section.hidden = ui.view !== 'settings' || section.dataset.section !== ui.section;
    }
    for (const button of byId('settings-nav').querySelectorAll('[data-open]')) {
      button.setAttribute('aria-current', String(ui.view === 'settings' && button.dataset.open === ui.section));
    }
    byId('settings-button').setAttribute('aria-current', String(ui.view === 'settings'));
  }

  // position places a popover under the button it belongs to.
  function position(popover) {
    const anchor = byId(popover.dataset.anchor);
    if (!anchor) {
      return;
    }
    const r = anchor.getBoundingClientRect();
    const width = document.documentElement.clientWidth;
    popover.style.top = Math.round(r.bottom + 6) + 'px';
    popover.style.maxHeight = Math.max(160, Math.round(window.innerHeight - r.bottom - 16)) + 'px';
    if (width < 600) {
      popover.style.left = '8px';
      popover.style.right = '8px';
    } else if (r.left + r.width / 2 > width / 2) {
      popover.style.left = 'auto';
      popover.style.right = Math.round(width - r.right) + 'px';
    } else {
      popover.style.left = Math.round(r.left) + 'px';
      popover.style.right = 'auto';
    }
  }

  // leaveArchived shows the first workstream of the list of work in place of
  // one just archived.
  function leaveArchived(id) {
    const next = streams().find((w) => !w.archived && w.workstream !== id);
    if (next) {
      select(next.workstream);
    }
  }

  // abandonWorkstream abandons a workstream with the owner's reason and,
  // when asked, archives it straight after.
  async function abandonWorkstream(workstream, reason, archive) {
    const out = await request('POST', '/abandon/' + workstream, { reason });
    if (archive) {
      await request('POST', '/archive/' + workstream);
    }
    return out;
  }

  // The confirmation dialog is one native dialog, filled for the action it
  // guards. It names the consequence, offers a quiet cancel beside the
  // danger button, closes on Escape and returns focus to the control that
  // opened it. The action runs only after the owner accepts.
  let confirming = null;

  // confirmDanger opens the dialog. options: title, consequence (paragraphs),
  // accept (the danger button's label), opener (the control that gets focus
  // back), run(reason) and optionally reason {label, hint, required}.
  function confirmDanger(options) {
    const dialog = byId('confirm-dialog');
    const form = byId('confirm-form');
    confirming = { ...options, accepted: false };
    byId('confirm-title').textContent = options.title;
    byId('confirm-consequence').replaceChildren(...options.consequence.map((text) => el('p', {}, text)));
    byId('confirm-accept').textContent = options.accept;
    const field = byId('confirm-reason-field');
    field.hidden = !options.reason;
    form.elements.reason.value = '';
    form.elements.reason.setAttribute('aria-required', String(!!options.reason));
    if (options.reason) {
      field.querySelector('label').textContent = options.reason.label;
      byId('confirm-reason-hint').textContent = options.reason.hint;
    }
    const result = byId('confirm-result');
    result.textContent = '';
    delete result.dataset.outcome;
    dialog.showModal();
    // Cancel is the safe default. A reason, when one is asked for, comes first.
    (options.reason ? form.elements.reason : byId('confirm-cancel')).focus();
  }

  function setupConfirmDialog() {
    const dialog = byId('confirm-dialog');
    const form = byId('confirm-form');
    byId('confirm-cancel').addEventListener('click', () => dialog.close('cancelled'));
    // A press on the backdrop is a press on the dialog element itself.
    dialog.addEventListener('click', (event) => {
      if (event.target === dialog) dialog.close('cancelled');
    });
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      if (!confirming) return;
      const reason = form.elements.reason.value.trim();
      if (confirming.reason && reason === '') {
        const result = byId('confirm-result');
        result.dataset.outcome = 'error';
        result.textContent = confirming.reason.required;
        form.elements.reason.focus();
        return;
      }
      confirming.accepted = true;
      confirming.reasonText = reason;
      dialog.close('accepted');
    });
    dialog.addEventListener('close', () => {
      // A close the browser reports after the dialog opened again belongs to
      // the earlier opening; the new one keeps its state.
      if (dialog.open) return;
      const done = confirming;
      confirming = null;
      if (!done) return;
      // The browser hands focus back to the control that was focused when the
      // dialog opened. A menu item that has since closed cannot take it, so
      // focus goes to the opener, unless the owner has already moved on.
      const active = document.activeElement;
      if (done.opener && done.opener.isConnected && (!active || active === document.body || active === dialog)) done.opener.focus();
      if (done.accepted) done.run(done.reasonText);
    });
  }

  // workstreamAction runs an entry of the workstream's menu.
  function workstreamAction(action) {
    const w = shown();
    if (!w) {
      return;
    }
    byId('workstream-menu').hidePopover();
    if (action === 'pause') {
      renderPauseForm();
      byId('pause-form').elements.target.value = 'workstream:' + w.workstream;
      byId('pause-popover').showPopover();
      return;
    }
    if (action === 'archive') {
      confirmDanger({
        title: 'Archive ' + goal(w) + ' permanently?',
        consequence: ['All workstream history, decisions, agent turns and check output will be deleted. Only its title will remain in the archived list. This cannot be undone.'],
        accept: 'Archive permanently',
        opener: byId('workstream-menu-button'),
        run: () => {
          const button = document.querySelector('[data-workstream-action="archive"]');
          act(inboxResult, button, () => request('POST', '/archive/' + w.workstream), (out) => {
            if (out.archived) {
              leaveArchived(w.workstream);
            }
            mark(['status', 'runtime']);
            return 'Archived ' + goal(w) + '; it is listed under Archived. Its history will be permanently deleted.';
          });
        },
      });
      return;
    }
    if (action === 'abandon' || action === 'abandon-archive') {
      const archive = action === 'abandon-archive';
      confirmDanger({
        title: (archive ? 'Abandon and archive ' : 'Abandon ') + goal(w) + '?',
        consequence: [
          'Abandoning stops new turns, cancels active turns while keeping their recorded work, and releases capacity. The trace is kept. Your branch and any open pull request are not touched.',
          ...(archive ? ['It is then archived permanently: all history, decisions, agent turns and check output are deleted and only the title remains. This cannot be undone.'] : []),
        ],
        reason: { label: 'Reason', hint: 'Why this work is stopping. It is recorded with the abandonment.', required: 'Give a reason for abandoning this workstream.' },
        accept: archive ? 'Abandon and archive' : 'Abandon workstream',
        opener: byId('workstream-menu-button'),
        run: (reason) => {
          const button = document.querySelector('[data-workstream-action="' + action + '"]');
          act(inboxResult, button, () => abandonWorkstream(w.workstream, reason, archive), () => {
            mark(['status', 'config', 'inbox', 'charter']);
            if (!archive) {
              return 'Abandoned ' + goal(w) + '.';
            }
            leaveArchived(w.workstream);
            return 'Abandoned and archived ' + goal(w) + '.';
          });
        },
      });
      return;
    }
    if (action === 'rebase') {
      const button = document.querySelector('[data-workstream-action="rebase"]');
      actOwner(button, () => request('POST', '/rebase/' + w.workstream), (out) => {
        mark(['status', 'inbox', 'feed/' + w.workstream]);
        return 'Drift rebase ' + out.drift + ' of ' + goal(w) + ' is requested; the foreman runs it once no landing or other drift rebase holds the project.';
      });
      return;
    }
    if (action === 'merged') {
      const button = document.querySelector('[data-workstream-action="merged"]');
      actOwner(button, () => request('POST', '/delivery/' + w.workstream + '/merged'), (out) => {
        mark(['status', 'runtime', 'inbox', 'feed/' + w.workstream]);
        return goal(w) + ' is delivered: ' + out.merge.upstream.remote + '/' + out.merge.upstream.branch + ' holds ' + out.merge.branch + '.';
      });
      return;
    }
    setTab('documents');
    const target = byId(action === 'dependency' ? 'base-form' : 'workstream-action');
    if (action === 'debate') {
      target.elements.action.focus();
    }
    target.scrollIntoView({ block: 'start' });
  }

  function setupLayout() {
    setupConfirmDialog();
    byId('problems-retry').addEventListener('click', () => retryReads(JSON.parse(byId('problems-retry').dataset.names || '[]')));
    byId('picker-toggle').addEventListener('click', () => {
      setPicker(document.body.dataset.picker !== 'open');
      render();
    });
    byId('picker-close').addEventListener('click', () => {
      setPicker(false);
      render();
    });
    for (const button of document.querySelectorAll('[data-open]')) {
      button.addEventListener('click', () => openView(button.dataset.open));
    }
    byId('settings-button').addEventListener('click', () => {
      openView(ui.view === 'settings' ? ui.returnView || 'project-pane' : 'settings');
    });
    for (const button of document.querySelectorAll('[data-close]')) {
      button.addEventListener('click', () => openView(ui.returnView || 'project-pane'));
    }
    for (const button of document.querySelectorAll('[data-tab]')) {
      button.addEventListener('click', () => setTab(button.dataset.tab));
    }
    for (const button of document.querySelectorAll('[data-workstream-action]')) {
      button.addEventListener('click', () => workstreamAction(button.dataset.workstreamAction));
    }
    for (const popover of document.querySelectorAll('[popover]')) {
      popover.addEventListener('beforetoggle', (event) => {
        if (event.newState === 'open') {
          position(popover);
        }
      });
    }
    window.addEventListener('resize', () => {
      for (const popover of document.querySelectorAll('[popover]')) {
        if (popover.matches(':popover-open')) {
          position(popover);
        }
      }
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && document.body.dataset.picker === 'open') {
        setPicker(false);
        render();
      }
    });
  }

  const kinds = {
    escalation: 'Question',
    ratification: 'Ratification',
    contested: 'Contested unit',
    amendment: 'Amendment',
    delivery: 'Mark ready for review',
    publication: 'Publication failing',
    checks: 'Checks not completing',
    'checks-choice': 'Checks missing',
    drift: 'Drift held',
    notices: 'Notices held',
    loop: 'Loop guard',
    base: 'Base parked',
    'project-creation': 'Project creation stuck',
    recovery: 'Approve this recovery move',
  };

  // pinNames name the request fields an entry's answer carries to pin what
  // the owner read.
  const pinNames = {
    spec: 'spec revision',
    plan: 'plan revision',
    packet: 'packet revision',
    review: 'final review',
    review_revision: 'report revision',
    commit: 'commit',
    draft_hash: 'draft',
    base: 'base workstream',
    accept_blocking: 'accepts blocking findings',
    push: 'push',
    pull_request: 'pull request',
    pushed_commit: 'commit',
    operation: 'push record',
    offer: 'offer',
    occurrence: 'move',
    subject: 'subject',
    from: 'from state',
    version: 'version',
    target: 'target',
    unit: 'unit',
  };

  const decisionNames = {
    review: 'Review the unit again',
    revise: 'Revise the unit',
    approve: 'Approve',
    reject: 'Reject',
    keep_waiting: 'Keep waiting',
    continue: 'Continue without check results',
    round: 'Debate another round',
    overrule: 'Overrule the objections',
    upstream: 'Continue from upstream',
    retry: 'Retry',
  };

  function workstreamName(id) {
    const w = views.status ? views.status.workstreams.find((x) => x.workstream === id) : undefined;
    return w ? goal(w) : id;
  }

  // pins states what an entry's answer is pinned to: the entry, unit or
  // amendment its endpoint names, and the revisions and identity its
  // request carries.
  function pins(entry) {
    const out = [];
    if (entry.kind === 'escalation') {
      out.push('inbox entry ' + entry.number);
    } else if (entry.kind === 'contested') {
      out.push('unit ' + entry.unit);
    } else if (entry.kind === 'amendment') {
      out.push('amendment ' + entry.amendment);
    }
    const body = entry.answer.body;
    const fields = Object.keys(pinNames).filter((field) => field in body);
    for (const field of [...fields, ...Object.keys(body).filter((field) => !(field in pinNames))]) {
      out.push((pinNames[field] || field) + ' ' + String(body[field]).slice(0, 12));
    }
    return out.join(', ');
  }

  function decisionKey(entry) {
    const key = [entry.kind, entry.project, entry.workstream, entry.number, entry.unit, entry.amendment].join(':');
    // A recovery card is told apart by the occurrence it was opened for.
    return entry.recovery ? key + ':' + entry.recovery.occurrence : key;
  }

  // decisions keeps each inbox entry's card between renders, so an answer
  // being written survives the events that
  // arrive meanwhile. A card answers the entry as it last rendered it.
  const decisions = new Map();

  // DISMISS_TIMEOUT_MS bounds how long a feed action item may stay hidden
  // after its submission is acknowledged. If the backend is still listing
  // it once this elapses, the dismissal clears and the item comes back as
  // actionable, so a submission that never finishes processing can never
  // hide an item for good.
  const DISMISS_TIMEOUT_MS = 120000;

  // dismissed holds, for each inbox entry identity a submission is
  // deciding, keyed the way decisionKey keys a card: null while the
  // submission is still in flight, and the time (Date.now()) the server
  // acknowledged it once that happens. inboxOf hides any entry whose key is
  // here, so the item disappears from the feed as soon as it is submitted,
  // before the server answers. pruneInbox clears a key once a refreshed
  // inbox no longer lists the entry, and expireDismissals clears one whose
  // acknowledgement is older than DISMISS_TIMEOUT_MS. Date.now is read
  // directly, rather than through some indirection, so a test can fake the
  // clock by overriding it on the page.
  const dismissed = new Map();

  function dismiss(key) {
    dismissed.set(key, null);
  }

  // acknowledge starts an already-dismissed entry's comeback window, once
  // the server has confirmed its submission.
  function acknowledge(key) {
    if (dismissed.has(key)) {
      dismissed.set(key, Date.now());
    }
  }

  // restore cancels a dismissal at once, used when a submission fails.
  function restore(key) {
    dismissed.delete(key);
  }

  // expireDismissals brings back any entry whose submission was
  // acknowledged at least DISMISS_TIMEOUT_MS ago and that the backend is
  // still listing, which is the only reason its key would still be here.
  function expireDismissals() {
    const now = Date.now();
    let changed = false;
    for (const [key, acknowledgedAt] of dismissed) {
      if (acknowledgedAt !== null && now - acknowledgedAt >= DISMISS_TIMEOUT_MS) {
        dismissed.delete(key);
        changed = true;
      }
    }
    if (changed) {
      render();
    }
  }

  // submit sends an answer and, once the API confirms it, clears the card's
  // form back to its default values, the way the browser's own reset does,
  // so a card the inbox still lists or reuses next never shows a value from
  // a stale submission; afterReset fixes up bookkeeping a field-by-field
  // reset would not, such as the delivery card's drafted-value tracking. A
  // refusal reads the inbox again, so the entry shows what the refusal was
  // about and the owner's input stands; the page never sends it again
  // itself.
  //
  // When dismissKey is given, the entry it names is hidden from the feed
  // at once, before the server answers; it stays hidden once the server
  // confirms, until a refreshed inbox no longer lists it or
  // DISMISS_TIMEOUT_MS passes, and it comes back at once, with the
  // owner's input kept, if the submission fails.
  function submit(form, button, method, path, body, done, afterReset, dismissKey) {
    if (dismissKey) {
      dismiss(dismissKey);
      render();
    }
    return actOwner(button, () => request(method, path, body), (out) => {
      if (dismissKey) {
        acknowledge(dismissKey);
      }
      const text = done(out);
      form.reset();
      if (afterReset) {
        afterReset();
      }
      return text;
    }, () => {
      if (dismissKey) {
        restore(dismissKey);
        render();
      }
      mark(['inbox']);
    });
  }

  function endpoint(entry) {
    return entry.answer.path.slice(api.length);
  }

  // landMessages is what the page says once Approve and land is accepted, for
  // each outcome the service reports: the owner is told what the landing did,
  // and that no further decision is needed.
  const landMessages = {
    landed: (name) => 'Landed ' + name + ' on its base branch.',
    held: (name) => 'The landing of ' + name + ' is held for now; nothing was pushed, and it lands once the hold clears.',
    'fell-back': (name) => 'The upstream refused the landing push of ' + name + '; its draft pull request was marked ready for review instead.',
    'base-moved': (name) => 'The base branch moved, so ' + name + ' is rebased and checked again before it lands.',
    pending: (name) => 'The landing of ' + name + ' is being settled against the base branch.',
  };

  // land approves the delivery and lands the workstream, through the answer
  // the entry offers for it, with the same pins as marking ready.
  function land(d) {
    const entry = d.entry;
    if (d.pending || !entry.land || !presented(entry, detailOf(entry))) return;
    d.pending = true;
    block(d.submit, true);
    block(d.land, true);
    submit(d.form, d.land, entry.land.method, entry.land.path.slice(api.length), { ...entry.land.body },
      (out) => (landMessages[out.outcome] || landMessages.pending)(workstreamName(entry.workstream)),
      undefined, decisionKey(entry)).finally(() => {
      d.pending = false;
      const view = presented(d.entry, detailOf(d.entry));
      block(d.submit, !view);
      block(d.land, !view);
    });
  }

  function accept(d) {
    const entry = d.entry;
    submit(d.form, d.accept, entry.answer.method, endpoint(entry), { ...entry.answer.body, text: entry.quick_reply },
      (out) => 'Accepted the recommendation on inbox entry ' + out.number + '.', undefined, decisionKey(entry));
  }

  function decide(d, choice, button) {
    if (d.pending) return;
    const entry = d.entry;
    const result = inboxResult;
    const body = { ...entry.answer.body };
    if (entry.kind === 'escalation') {
      const text = d.text.value.trim();
      if (text === '') {
        show(result, 'error', 'Write an answer first.');
        return;
      }
      submit(d.form, d.submit, entry.answer.method, endpoint(entry), { ...body, text },
        (out) => 'Answered inbox entry ' + out.number + '.', undefined, decisionKey(entry));
      return;
    }
    if (entry.kind === 'project-selection') {
      body.projects = d.text.value.split(/[\s,]+/).filter(Boolean);
      submit(d.form, d.submit, entry.answer.method, endpoint(entry), body, () => 'Projects selected. The start will continue.', undefined, decisionKey(entry));
      return;
    }
    if (entry.kind === 'delivery') {
      // The body is the entry's pinned identity alone: the service then
      // puts the text the card previews on the pull request.
      d.pending = true;
      block(d.submit, true);
      block(d.land, true);
      submit(d.form, d.submit, entry.answer.method, endpoint(entry), body,
        (out) => 'Approved marking final review ' + out.review + ' of ' + workstreamName(entry.workstream) + ' ready for review.',
        undefined, decisionKey(entry)).finally(() => {
        d.pending = false;
        block(d.submit, !presented(d.entry, detailOf(d.entry)));
        block(d.land, !presented(d.entry, detailOf(d.entry)));
      });
      return;
    }
    // A form submit from the note field makes no choice.
    if (!choice) return;
    const [decision, objection] = choice.split(' ');
    const note = d.note.value.trim();
    const send = (method, path, body, done, dismissKey) => {
      d.pending = true;
      for (const choiceButton of d.node.querySelectorAll('[data-decision-choice]')) block(choiceButton, true);
      return submit(d.form, button, method, path, body, done, undefined, dismissKey).finally(() => {
        d.pending = false;
        for (const choiceButton of d.node.querySelectorAll('[data-decision-choice]')) block(choiceButton, false);
      });
    };
    switch (entry.kind) {
      case 'contested':
        if (note === '') {
          show(result, 'error', 'Give a note for the ruling.');
          return;
        }
        send(entry.answer.method, endpoint(entry), { ...body, decision, note },
          () => 'Ruled ' + decision + ' on unit ' + entry.unit + '.', decisionKey(entry));
        return;
      case 'amendment':
        if (note !== '') {
          body.note = note;
        }
        send(entry.answer.method, endpoint(entry), { ...body, decision },
          () => 'Decided ' + decision + ' on amendment ' + entry.amendment + '.', decisionKey(entry));
        return;
      case 'project-creation':
        // The retry finishes the creation under its identity, reusing only the
        // repository that creation made.
        send(entry.answer.method, endpoint(entry), body,
          () => 'The project was created and registered.', decisionKey(entry));
        return;
      case 'checks-choice':
        send(entry.answer.method, endpoint(entry), { ...body, decision },
          (out) => out.decision === 'continue' ? 'The final review of push ' + out.push + ' continues without check results.' : 'Still waiting for the check results of push ' + out.push + '.', decisionKey(entry));
        return;
      case 'recovery':
        // The body is the pinned move alone: the service applies exactly that
        // move, or refuses it as stale.
        send(entry.answer.method, endpoint(entry), body,
          (out) => 'Approved: ' + out.detail, decisionKey(entry));
        return;
      case 'base':
        send(entry.answer.method, endpoint(entry), body,
          (out) => workstreamName(entry.workstream) + ' continues from ' + out.upstream.remote + '/' + out.upstream.branch + '.', decisionKey(entry));
        return;
    }
    // A ratification is decided with ratify, which dismisses its card like
    // any other terminal decision, or through the shed: a disposition of
    // an objection or a request for a redraft, which the shed may still
    // leave open with updated dissent, so those do not dismiss it.
    if (decision === 'ratify') {
      const packet = views[detailOf(entry)];
      if (packet && packet.project_spec_revision) {
        body.project_spec_revision = packet.project_spec_revision;
      }
      send(entry.answer.method, endpoint(entry), body, (out) => out.detail, decisionKey(entry));
      mark([detailOf(entry)]);
      return;
    }
    if (decision === 'sustain') {
      send('POST', '/shed/rule/' + entry.workstream, { objection, disposition: 'sustain', note }, (out) => out.detail);
      return;
    }
    if (decision === 'overrule') {
      send('POST', '/shed/overrule/' + entry.workstream, { objection, reason: note }, (out) => out.detail);
      return;
    }
    if (decision === 'redraft') {
      if (note === '') {
        show(result, 'error', 'Say what the redraft should change.');
        return;
      }
      send('POST', '/shed/redraft/' + entry.workstream, { note }, (out) => out.detail);
      return;
    }
  }

  function decisionCard(entry) {
    const key = decisionKey(entry);
    let d = decisions.get(key);
    if (d) {
      return d;
    }
    d = {
      entry,
      head: el('div', { class: 'summary' }),
      detail: el('div', { 'data-field': 'detail' }),
      actions: el('div', { class: 'actions' }),
      submit: el('button', { type: 'submit', class: 'primary' }, entry.kind === 'delivery' ? deliveryLabel(entry, null) : entry.kind === 'escalation' ? 'Answer' : 'Decide'),
      shown: null,
      opened: {},
    };
    const fields = [];
    if (entry.kind === 'escalation') {
      d.text = el('textarea', { name: 'text', rows: '2', 'aria-label': 'Your answer' });
      d.accept = el('button', { type: 'button', 'data-field': 'accept' });
      d.accept.addEventListener('click', () => accept(d));
      fields.push(d.text);
    } else if (entry.kind === 'project-selection') {
      d.text = el('textarea', { name: 'projects', rows: '2', required: true });
      fields.push(el('label', {}, 'Project IDs in order, separated by spaces or commas', d.text));
    } else if (entry.kind === 'delivery') {
      // The preview is one node kept across renders, so it stays open or
      // collapsed as the owner left it.
      d.preview = el('details', { 'data-field': 'preview' }, el('summary', {}, 'What will be published'));
      fields.push(d.preview);
      d.authorization = el('p', { class: 'meta', 'data-field': 'authorization' }, 'Approving puts the description above on the draft pull request, with the brief\'s "Before you ship" items, and marks the pull request ready for review.');
      fields.push(d.authorization);
      // The landing button belongs to a project that lands, which the entry
      // says by offering the landing as an answer.
      d.land = el('button', { type: 'button', class: 'secondary', 'data-field': 'land' }, 'Approve and land');
      d.land.addEventListener('click', () => land(d));
    } else {
      d.note = el('input', { name: 'note', type: 'text', autocomplete: 'off' });
      fields.push(el('label', {}, 'Note', d.note));
    }
    const form = el('form', { class: 'send', 'data-field': 'answer' }, ...fields, d.actions);
    d.form = form;
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      decide(d);
    });
    // A failing publication, a held drift rebase, held notices or a loop
    // guard pause take no decision here, so their cards have no form.
    d.node = el('article', { class: 'decision', 'data-decision': key, 'data-kind': entry.kind, 'data-workstream': entry.workstream }, d.head, d.detail, ['publication', 'checks', 'drift', 'notices', 'loop'].includes(entry.kind) ? null : form);
    decisions.set(key, d);
    return d;
  }

  // renderRecoveryMove lays out the move a recovery card pins, which the
  // button applies exactly: what changes, from which state and version, to
  // which target, and why the owner is asked.
  function renderRecoveryMove(m) {
    const row = (name, value) => [el('dt', {}, name), el('dd', { 'data-field': name.toLowerCase() }, value)];
    return el('dl', { class: 'move', 'data-field': 'move' },
      ...row('Subject', m.unit ? 'Unit ' + m.unit : 'Workstream'),
      ...row('From', m.from + ', version ' + m.version),
      ...row('Target', m.target),
      ...row('Reason', m.reason));
  }

  function renderDissent(d, name) {
    const view = views[name];
    if (!view) {
      d.detail.replaceChildren(readState(name, 'Reading the packet…', 'The packet is unavailable.'));
      return [];
    }
    // The chief of staff decides the committee's objections first; the
    // owner rules on what it escalated and on their own, and what it
    // dismissed is folded away with its reasons.
    const all = view.packet.dissent || [];
    const dissent = all.filter((o) => o.by !== 'chief_of_staff');
    const dismissed = all.filter((o) => o.by === 'chief_of_staff');
    d.detail.replaceChildren(...[
      view.project_spec ? el('details', { 'data-field': 'project-spec' },
        el('summary', {}, 'Workgroup spec revision ' + view.project_spec_revision),
        el('pre', {}, JSON.stringify(view.project_spec, null, 2))) : null,
      dissent.length === 0 ? el('p', { class: 'meta' }, 'No objection stands.') : el('ul', { class: 'dissent' }, ...dissent.map((o) => el('li', { 'data-objection': o.id },
        el('div', { class: 'line' },
          el('strong', { class: 'mono' }, o.id),
          el('span', { class: 'tag', 'data-field': 'standing' }, [o.blocking ? 'blocking' : 'advice', o.disposition].filter(Boolean).join(', '))),
        el('div', { class: 'meta' }, o.kind + ' by ' + o.member + ' in round ' + o.round + (o.part ? ' on ' + o.part : '')),
        el('div', { 'data-field': 'argument' }, o.argument),
        o.escalation ? el('div', { class: 'meta', 'data-field': 'escalation' }, 'Chief of staff: ' + o.escalation) : null,
        o.note ? el('div', { class: 'meta', 'data-field': 'note' }, 'Your note: ' + o.note) : null,
        // Each objection is ruled on by its own pair of buttons, which send
        // the card's note with the objection they sit under.
        el('div', { class: 'objection-actions', role: 'group', 'aria-label': 'Rule on ' + o.id, 'data-field': 'objection-actions' },
          choiceButton(d, 'sustain ' + o.id, 'Sustain', 'secondary', 'Sustain ' + o.id),
          choiceButton(d, 'overrule ' + o.id, 'Overrule', 'secondary', 'Overrule ' + o.id))))),
      dissent.length === 0 ? null : el('p', { class: 'meta', 'data-field': 'objection-hint' }, 'Sustain, overrule and redraft send the note below.'),
      dismissed.length === 0 ? null : el('details', { 'data-field': 'dismissed' },
        el('summary', { class: 'meta' }, 'Dismissed by the chief of staff (' + dismissed.length + ')'),
        el('ul', { class: 'dissent' }, ...dismissed.map((o) => el('li', { 'data-objection': o.id },
          el('div', { class: 'meta' }, o.id + ': ' + o.kind + ' by ' + o.member + (o.part ? ' on ' + o.part : '')),
          el('div', { 'data-field': 'argument' }, o.argument),
          el('div', { class: 'meta', 'data-field': 'note' }, 'Chief of staff: ' + o.note)))))].filter(Boolean));
    return dissent;
  }

  // presented returns the delivery view when it presents the final report
  // and draft the entry pins, and null otherwise: before its first read, and
  // while a read for the entry's newer pins is pending or failed.
  function presented(entry, name) {
    const view = views[name];
    const pin = entry.answer.body;
    return view && view.report.review === pin.review && view.review_revision === pin.review_revision &&
      view.report.commit === pin.commit && view.draft_hash === pin.draft_hash ? view : null;
  }

  // renderDelivery shows the final report and the draft the entry pins.
  // The button waits until they are shown, so an approval never pins a draft
  // the page does not show.
  function renderDelivery(d, name) {
    const view = presented(d.entry, name);
    block(d.submit, !view || !!d.pending);
    block(d.land, !view || !!d.pending);
    d.preview.hidden = !view;
    if (!view) {
      d.detail.replaceChildren(el('div', { 'data-field': 'reading' }, readState(name, 'Reading the final report…', 'The final report is unavailable.')));
      return;
    }
    // The title is the one an already-approved delivery records, even if the
    // workstream's title changes later.
    const title = view.approval ? (view.approval.title || view.title) : view.title;
    const findings = view.report.findings || [];
    // The card reads as a release note: the brief, the findings, the
    // criteria, then what will be published, and the button last.
    d.detail.replaceChildren(...[
      renderDeliveryBrief(view),
      el('p', { class: 'meta', 'data-field': 'title' }, 'Pull request title: ' + title),
      renderPullRequest(view),
      view.draft_pull_request ? el('p', { class: 'meta', 'data-field': 'draft-note' }, 'A merge-order dependency has not merged. Approving records your decision and the pull request stays a draft.') : null,
      renderDeliveryChecks(view),
      view.report.summary || findings.length > 0 ? el('section', { class: 'release-section', 'data-field': 'review' },
        el('h4', {}, 'Findings'),
        view.report.summary ? el('p', { 'data-field': 'summary' }, view.report.summary) : null,
        findings.length === 0 ? null : el('ul', { class: 'findings', 'data-field': 'findings' }, ...findings.map((f) => el('li', { 'data-severity': f.severity },
          el('span', { class: 'tag' }, f.severity), ' ', (f.criteria || []).length ? f.criteria.join(', ') + ': ' : '', f.finding,
          f.action ? el('div', { class: 'meta' }, 'Action: ' + f.action) : null)))) : null,
      el('section', { class: 'release-section', 'data-field': 'criteria-section' },
        el('h4', {}, 'Criteria'),
        el('ul', { class: 'criteria', 'data-field': 'criteria' }, ...view.report.criteria.map((c) => el('li', { 'data-criterion': c.criterion },
          el('strong', { class: 'mono' }, c.criterion), ' ', c.text,
          el('div', { class: 'meta' }, c.evidence ? c.evidence : 'Gap: ' + c.gap))))),
    ].filter((node) => node !== null));
    // What ApproveDelivery publishes when the page sends no text: the text
    // of the approval the presentation carries, and the draft otherwise.
    const description = view.approval ? view.approval.description : view.draft;
    const messages = view.approval ? view.approval.messages : (view.messages || []);
    d.preview.replaceChildren(
      d.preview.querySelector('summary'),
      el('p', { class: 'meta', 'data-field': 'title' }, 'Pull request title: ' + title),
      el('div', { class: 'meta' }, 'Pull request description'),
      el('pre', { 'data-field': 'description' }, description),
      ...messages.flatMap((m, i) => [
        el('div', { class: 'meta' }, 'Commit message' + (messages.length > 1 ? ' ' + (i + 1) : '')),
        el('pre', { 'data-field': 'commit-message' }, m.message)]));
    d.submit.textContent = deliveryLabel(d.entry, view);
    d.land.textContent = d.entry.answer.body.accept_blocking ? 'Accept findings and land' : 'Approve and land';
    // Jev's recommendation, when it made one without the authority to apply
    // it, marks the route it favours as the primary action.
    const landFirst = view.recommendation === 'land';
    d.land.className = landFirst ? 'primary' : 'secondary';
    d.submit.className = landFirst ? 'secondary' : 'primary';
    d.authorization.textContent = 'Approving puts the description above on the draft pull request, with the brief\'s "Before you ship" items, and marks the pull request ready for review.' +
      (d.entry.land ? ' Approve and land moves the base branch to the checked commit instead, and opens no pull request.' : '');
  }

  // renderPullRequest names the draft pull request the decision is about: the
  // pull request of the latest recorded push.
  function renderPullRequest(view) {
    const p = view.publication;
    if (!p || !p.pull_request) return null;
    return el('p', { class: 'meta', 'data-field': 'pull-request' }, 'Pull request ',
      p.url ? el('a', { href: p.url, target: '_blank', rel: 'noopener noreferrer' }, '#' + p.pull_request) : '#' + p.pull_request);
  }

  // renderDeliveryChecks is the check result block of a delivery card: the
  // result recorded for the commit the final review read.
  function renderDeliveryChecks(view) {
    const c = view.checks;
    if (!c) return null;
    return el('p', { 'data-field': 'checks', 'data-checks-state': c.state }, c.summary);
  }

  // deliveryLabel names what approving the entry's delivery does: it updates
  // the pull request of a delivered workstream being maintained and marks
  // the draft pull request ready for review otherwise. Accepting the final
  // review's blocking findings comes first in the label.
  function deliveryLabel(entry, view) {
    const action = view && view.maintenance ? 'update pull request' : 'mark ready for review';
    if (entry.answer.body.accept_blocking) {
      return 'Accept findings and ' + action;
    }
    return action.charAt(0).toUpperCase() + action.slice(1);
  }

  // recommendedChoice names the option an entry's recommendation points at:
  // the one it opens with ("ratify: no objection stands"). With no
  // recommendation to read, a lone option is the recommended one. A
  // contested unit's note is the chief of staff's account, not a pick.
  function recommendedChoice(entry) {
    const options = entry.options || [];
    const text = (entry.recommendation || '').trim().toLowerCase();
    if (entry.kind !== 'contested' && text !== '') {
      const lead = text.split(/[:.]/)[0].trim();
      return options.find((o) => o.toLowerCase() === lead) || '';
    }
    return options.length === 1 ? options[0] : '';
  }

  // choiceButton is one decision of a card: primary when it is the
  // recommendation, secondary otherwise. label is what it says and name,
  // when given, is the longer name assistive technology reads.
  function choiceButton(d, choice, label, variant, name) {
    const button = el('button', { type: 'button', class: variant, 'data-decision-choice': choice, ...(name ? { 'aria-label': name } : {}) }, label);
    block(button, !!d.pending);
    button.addEventListener('click', () => decide(d, choice, button));
    return button;
  }

  function decisionButtons(d, options) {
    const key = JSON.stringify(options);
    if (d.actions.dataset.options === key) return;
    d.actions.dataset.options = key;
    place(d.actions, options.map(([choice, label, variant]) => choiceButton(d, choice, label, variant)));
  }

  function renderDecision(entry) {
    const d = decisionCard(entry);
    d.entry = entry;
    const name = detailOf(entry);
    const key = JSON.stringify([entry, name && views[name], name && !!failures[name], workstreamName(entry.workstream)]);
    if (d.shown === key) {
      return d.node;
    }
    d.shown = key;
    const recommended = recommendedChoice(entry);
    const variant = (option) => (option === recommended ? 'primary' : 'secondary');
    let options = null;
    if (entry.kind === 'escalation') {
      options = entry.options.length === 0 ? null : el('div', { class: 'choices', role: 'group', 'aria-label': 'Options', 'data-field': 'options' }, ...entry.options.map((o) => {
        const button = el('button', { type: 'button', class: 'secondary', 'data-field': 'option' }, o);
        button.addEventListener('click', () => {
          d.text.value = o;
        });
        return button;
      }));
    } else if (entry.options.length === 0) {
      const none = ['ratification', 'publication', 'checks', 'drift', 'notices', 'loop', 'base'].includes(entry.kind) ? 'none until what blocks it is resolved' : 'none';
      options = el('p', { class: 'meta', 'data-field': 'options' }, 'Options: ' + none);
    }
    // The details a card carries are footnotes: closed, small and mono, and
    // kept open or closed as the owner left them across repaints.
    const footnote = (name, ...parts) => {
      const note = el('details', { class: 'footnote ' + name, 'data-field': 'footnote-' + name }, ...parts);
      note.open = !!d.opened[name];
      note.addEventListener('toggle', () => {
        d.opened[name] = note.open;
      });
      return note;
    };
    d.head.replaceChildren(...[
      // The question comes first; what kind of card it is and when it was
      // raised follow in a quiet row with the footnotes.
      el('p', { class: 'question', 'data-field': 'question' }, entry.question),
      el('div', { class: 'card-meta' },
        el('span', { class: 'tag', 'data-field': 'kind' }, kinds[entry.kind] || entry.kind),
        el('span', { class: 'meta', 'data-field': 'opened' }, when(entry.opened_at)),
        // The question above is the chief of staff's rephrasing; what each
        // role asked in its own words stays one tap away.
        entry.asked.length === 0 ? null : footnote('asked',
          el('summary', {}, entry.asked.length === 1 ? 'What was asked' : 'What was asked (' + entry.asked.length + ' questions)'),
          el('ul', { 'data-field': 'asked' }, ...entry.asked.map((q) => el('li', {},
            el('span', { class: 'meta' }, q.asked_by + (q.unit ? ' on ' + q.unit : '') + ': '), q.question)))),
        ['publication', 'checks'].includes(entry.kind) ? null : footnote('revisions',
          el('summary', {}, 'Details'),
          el('p', { class: 'mono', 'data-field': 'pins' }, 'Answers ' + pins(entry)))),
      entry.blocked ? el('p', { class: 'meta', 'data-field': 'blocked' }, 'Waiting on it: ' + entry.blocked) : null,
      entry.recommendation ? el('p', { class: 'recommendation', 'data-field': 'recommendation' }, (entry.kind === 'contested' ? 'Note: ' : 'Recommendation: ') + entry.recommendation) : null,
      options,
    ].filter((node) => node !== null));
    d.actions.removeAttribute('data-field');
    switch (entry.kind) {
      case 'escalation':
        d.accept.textContent = 'Accept: ' + entry.quick_reply;
        d.accept.className = 'primary';
        d.submit.className = entry.quick_reply ? 'secondary' : 'primary';
        place(d.actions, entry.quick_reply ? [d.accept, d.submit] : [d.submit]);
        break;
      case 'project-selection':
        place(d.actions, [d.submit]);
        break;
      case 'delivery':
        renderDelivery(d, name);
        d.actions.setAttribute('data-field', 'options');
        place(d.actions, entry.land ? [d.submit, d.land] : [d.submit]);
        break;
      case 'publication':
      case 'checks':
        break;
      case 'ratification': {
        renderDissent(d, name);
        d.actions.setAttribute('data-field', 'options');
        decisionButtons(d, [
          ...(entry.options.includes('ratify') ? [['ratify', 'Ratify', variant('ratify')]] : []),
          ['redraft', 'Ask for a redraft', 'secondary']]);
        break;
      }
      default:
        d.actions.setAttribute('data-field', 'options');
        decisionButtons(d, entry.options.map((o) => [o, entry.kind === 'recovery' ? 'Approve this move' : decisionNames[o] || o, variant(o)]));
        if (entry.kind === 'recovery') {
          d.detail.replaceChildren(renderRecoveryMove(entry.recovery));
        }
        // A parked base takes no note, and no decision here unless its own
        // base was abandoned.
        if (entry.kind === 'base') {
          d.form.hidden = entry.options.length === 0;
          d.note.parentElement.hidden = true;
        } else if (entry.kind === 'project-creation' || entry.kind === 'checks-choice' || entry.kind === 'recovery') {
          d.note.parentElement.hidden = true;
        }
    }
    return d.node;
  }

  // pruneInbox forgets the cards of entries and charter proposals the
  // inbox no longer lists, and clears the dismissal of any entry no longer
  // listed, which is what brings a dismissed item back once the server has
  // actually processed its submission.
  function pruneInbox() {
    if (views.inbox) {
      const keys = new Set(views.inbox.entries.map(decisionKey));
      for (const key of decisions.keys()) {
        if (!keys.has(key)) {
          decisions.delete(key);
        }
      }
      for (const key of dismissed.keys()) {
        if (!keys.has(key)) {
          dismissed.delete(key);
        }
      }
    }
    if (views.charter) {
      const keys = new Set(views.charter.proposals.map(proposalKey));
      for (const key of proposals.keys()) {
        if (!keys.has(key)) {
          proposals.delete(key);
        }
      }
    }
  }

  function proposalKey(p) {
    return p.workstream + ':' + p.question;
  }

  // proposals keeps each charter proposal's card between renders.
  const proposals = new Map();

  function renderProposal(p) {
    const key = proposalKey(p);
    let card = proposals.get(key);
    if (!card) {
      const decide = (decision) => {
        const button = el('button', { type: 'button', 'data-field': decision }, decision === 'ratify' ? 'Ratify rule' : 'Decline rule');
        button.addEventListener('click', () => {
          actOwner(button, () => request('POST', '/charter/' + p.workstream + '/' + p.question, { decision }),
            () => {
              mark(['charter', 'config']);
              return 'Charter decision recorded.';
            });
        });
        return button;
      };
      card = { rule: el('p', { class: 'text', 'data-field': 'rule' }), response: el('p', { class: 'meta' }) };
      card.node = el('article', { class: 'decision', 'data-proposal': key, 'data-kind': 'charter' },
        el('div', { class: 'line' }, el('span', { class: 'tag' }, 'Charter rule')),
        card.rule, card.response, el('div', { class: 'actions' }, decide('ratify'), decide('decline')));
      proposals.set(key, card);
    }
    card.rule.textContent = p.rule;
    card.response.textContent = 'From your ruling: ' + p.owner_response;
    return card.node;
  }

  // priority is the order being edited: the workstreams in the order shown
  // and those chosen to go first. It starts again from the order in force
  // whenever that changes.
  const priority = { inForce: null, order: [], chosen: new Set(), shown: null };

  function priorityProject() {
    const id = byId('priority-project').value;
    return configuredRepos().some((p) => p.id === id) ? id : null;
  }

  function renderPriority() {
    const current = byId('priority-current');
    const list = byId('priority-list');
    if (!views.status || !views.runtime || !views.config) {
      return;
    }
    setOptions(byId('priority-project'), configuredRepos().map((p) => [p.id, p.label]));
    const id = priorityProject();
    block(byId('priority-form').querySelector('button[type=submit]'), !id);
    block(byId('priority-clear'), !id);
    if (!id) {
      current.textContent = 'No repo is configured.';
      list.replaceChildren();
      priority.inForce = null;
      priority.shown = null;
      priority.order = [];
      priority.chosen.clear();
      return;
    }
    const record = (views.runtime.effective.priorities || []).find((p) => p.project === id);
    const inForce = record ? record.workstreams : [];
    const key = JSON.stringify([id, inForce]);
    if (priority.inForce !== key) {
      priority.inForce = key;
      priority.order = [...inForce];
      priority.chosen = new Set(inForce);
    }
    const workstreams = views.status.workstreams.filter((w) => w.project === id && !w.archived);
    const ids = workstreams.map((w) => w.workstream);
    priority.order = priority.order.filter((w) => ids.includes(w)).concat(ids.filter((w) => !priority.order.includes(w)));
    for (const w of [...priority.chosen]) {
      if (!ids.includes(w)) {
        priority.chosen.delete(w);
      }
    }
    const goals = new Map(workstreams.map((w) => [w.workstream, goal(w)]));
    current.textContent = inForce.length === 0
      ? 'No order is set; free slots go to workstreams without preference.'
      : 'In force: ' + inForce.map((w) => goals.get(w) || w).join(', then ') + '.';
    const shown = JSON.stringify([id, priority.order, [...priority.chosen], [...goals]]);
    if (priority.shown === shown) {
      return;
    }
    priority.shown = shown;
    list.replaceChildren(...priority.order.map((w, i) => {
      const box = el('input', { type: 'checkbox', 'data-field': 'chosen' });
      box.checked = priority.chosen.has(w);
      box.addEventListener('change', () => {
        if (box.checked) {
          priority.chosen.add(w);
        } else {
          priority.chosen.delete(w);
        }
        renderPriority();
      });
      const move = (label, by) => {
        const button = el('button', { type: 'button', 'data-field': by < 0 ? 'up' : 'down', 'aria-label': label }, by < 0 ? '↑' : '↓');
        button.disabled = i + by < 0 || i + by >= priority.order.length;
        button.addEventListener('click', () => {
          const order = priority.order;
          [order[i], order[i + by]] = [order[i + by], order[i]];
          renderPriority();
        });
        return button;
      };
      return el('li', { 'data-priority': w },
        el('label', {}, box, ' ', goals.get(w), el('span', { class: 'id' }, ' ' + w)),
        el('span', { class: 'moves' }, move('Move up', -1), move('Move down', 1)));
    }));
  }

  function setPriority(event) {
    event.preventDefault();
    const result = byId('priority-result');
    const id = priorityProject();
    const workstreams = priority.order.filter((w) => priority.chosen.has(w));
    if (!id) {
      show(result, 'error', 'No repo is configured.');
      return;
    }
    if (workstreams.length === 0) {
      show(result, 'error', 'Choose the workstreams that go first, or clear the order.');
      return;
    }
    act(result, event.submitter || byId('priority-form').querySelector('button'), () => request('PUT', '/runtime/priority', { project: id, workstreams }),
      () => 'Priority order set.');
  }

  function clearPriority() {
    const result = byId('priority-result');
    const id = priorityProject();
    if (!id) {
      show(result, 'error', 'No repo is configured.');
      return;
    }
    act(result, byId('priority-clear'), () => request('DELETE', '/runtime/priority', { project: id }), () => 'Priority order cleared.');
  }

  function money(spend) {
    return 'USD ' + spend.spend_usd + (spend.lower_bound ? ' or more (' + spend.unknown_costs + ' unknown)' : '');
  }

  function usageOf(provider) {
    const usage = views.status && views.status.provider_usage;
    return usage ? usage.providers.find((p) => p.provider === provider) : undefined;
  }

  function limitText(limit) {
    return 'limited (' + [limit.status, limit.kind].filter(Boolean).join(', ') + ')' +
      (limit.resets_at && !limit.resets_at.startsWith('0001-') ? ' until ' + when(limit.resets_at) : '');
  }

  // profileRows keeps each role's row between renders, so a chosen profile
  // survives the events that arrive meanwhile.
  const profileRows = new Map();

  function profileRow(role) {
    let r = profileRows.get(role);
    if (r) {
      return r;
    }
    r = {
      profile: el('span', { 'data-field': 'profile' }),
      source: el('span', { class: 'tag', 'data-field': 'source' }),
      usage: el('div', { class: 'meta', 'data-field': 'usage' }),
      select: el('select', { name: 'profile', 'aria-label': 'Profile for ' + role }),
      set: el('button', { type: 'button', 'data-field': 'set' }, 'Set'),
      clear: el('button', { type: 'button', 'data-field': 'clear' }, 'Clear'),
    };
    const result = byId('profile-result');
    r.set.addEventListener('click', () => {
      const profile = r.select.value;
      if (!profile) {
        show(result, 'error', 'Choose a profile for ' + role + '.');
        return;
      }
      act(result, r.set, () => request('PUT', '/runtime/profile', { role, profile }), () => role + ' runs ' + profile + ' from its next turn.');
    });
    r.clear.addEventListener('click', () => {
      act(result, r.clear, () => request('DELETE', '/runtime/profile', { role }), () => role + ' runs its configured profile from its next turn.');
    });
    r.node = el('div', { class: 'role-profile', 'data-profile-role': role },
      el('div', { class: 'line' }, el('strong', { class: 'mono' }, role), el('span', {}, r.profile, ' ', r.source)),
      r.usage,
      el('div', { class: 'actions' }, r.select, r.set, r.clear));
    profileRows.set(role, r);
    return r;
  }

  function renderProfiles() {
    const box = byId('profiles');
    if (!views.runtime || !views.config || !views.config.effective) {
      return;
    }
    const configured = views.config.effective.profiles || {};
    const options = Object.keys(configured).sort().map((name) => [name, name + ', ' + configured[name].agent]);
    const roles = Object.keys(views.runtime.profiles || {}).sort();
    for (const role of profileRows.keys()) {
      if (!roles.includes(role)) {
        profileRows.delete(role);
      }
    }
    place(box, roles.map((role) => {
      const effective = views.runtime.profiles[role];
      const r = profileRow(role);
      const provider = configured[effective.name] ? configured[effective.name].agent : '';
      r.profile.textContent = effective.name ? effective.name + (provider ? ', ' + provider : '') : 'none';
      r.source.textContent = profileSources[effective.source] || effective.source;
      r.source.title = effective.reason || '';
      const usage = usageOf(provider);
      r.usage.textContent = usage
        ? provider + ': ' + money(usage) + ' today' + (usage.limit ? ', ' + limitText(usage.limit) : '')
        : '';
      setOptions(r.select, [['', 'Choose a profile'], ...options]);
      block(r.clear, effective.source !== 'owner_override');
      return r.node;
    }));
  }

  function clearProviderLimit(provider) {
    const button = el('button', {type: 'button'}, 'Clear provider limit');
    button.addEventListener('click', () => act(byId('provider-result'), button,
      () => request('DELETE', '/runtime/provider-limit', {backend: provider}),
      () => { mark(['status', 'runtime']); return 'Provider limit cleared.'; }));
    return button;
  }

  function renderProviders() {
    const list = byId('providers');
    const usage = views.status ? views.status.provider_usage : null;
    if (!usage) {
      list.replaceChildren(el('li', { class: 'meta' }, views.status ? 'Provider usage is unavailable.' : ''));
      return;
    }
    const rows = usage.providers.map((p) => el('li', { 'data-provider': p.provider },
      el('div', { class: 'line' }, el('strong', {}, p.provider), el('span', { 'data-field': 'spend' }, money(p))),
      p.limit ? el('div', { class: 'attention', 'data-field': 'limit' }, limitText(p.limit), clearProviderLimit(p.provider)) : null,
      ...p.fallbacks.map((f) => el('div', { class: 'meta' }, f.role + ' runs ' + f.profile + ' instead of ' + f.configured)),
      p.paused_roles.length > 0 ? el('div', { class: 'meta' }, 'Paused: ' + p.paused_roles.join(', ')) : null));
    if (usage.unattributed) {
      rows.push(el('li', {}, el('div', { class: 'line' }, el('strong', {}, 'unattributed'), el('span', {}, money(usage.unattributed)))));
    }
    list.replaceChildren(...rows, el('li', { class: 'meta' }, 'Known spend on ' + usage.day + '.'));
  }

  // reloadable reports whether a reload has something to do: a valid change
  // to apply, or a file to refuse. A change only a restart applies is not
  // one.
  function reloadable(config) {
    return config.diagnostics.some((d) => d.code === 'reload_required') ||
      config.drift.files.some((f) => f.state === 'invalid');
  }

  function renderConfig() {
    const config = views.config;
    if (!config) {
      return;
    }
    const digest = byId('digest');
    digest.textContent = config.digest.slice(0, 12);
    digest.dataset.digest = config.digest;
    byId('reload').dataset.lit = String(reloadable(config));
    byId('drift').textContent = config.drift.differs
      ? 'The configuration on disk differs from what is loaded.'
      : 'The configuration on disk matches what is loaded.';
    byId('drift-files').replaceChildren(...config.drift.files.map((f) => el('li', { 'data-file': f.path, 'data-state': f.state },
      el('span', { class: 'tag' }, f.state), ' ', el('span', { class: 'id' }, f.path),
      f.reason ? el('div', { class: 'meta' }, f.reason) : null)));
    const last = byId('last-error');
    last.hidden = !config.last_error;
    last.textContent = config.last_error ? 'The last reload failed at ' + when(config.last_error.at) + ': ' + config.last_error.message : '';
    byId('config-diagnostics').replaceChildren(...config.diagnostics.map((d) => el('li', { 'data-code': d.code }, d.message)));
  }

  function reload() {
    act(byId('reload-result'), byId('reload'), () => request('POST', '/reload'), (out) => {
      mark(['config']);
      const restart = out.restart_required || [];
      return 'Reloaded; loaded ' + out.digest.slice(0, 12) + '.' +
        (restart.length > 0 ? ' Restart the service to apply ' + restart.join(', ') + '.' : '');
    });
  }


  let charterRead = null;
  let charterProject = '';
  const isManagementView = name => name.startsWith('projects/charter/') || name.startsWith('projects/config/');
  const managementRepos = new Map();
  function receiveCharter(project, out, explicit = false) {
    const form = byId('project-edit');
    if (!explicit && charterRead && (charterRead.conflict || form.elements.content.value !== charterRead.content)) {
      if (out.revision !== charterRead.revision) charterRead.conflict = true;
    } else {
      charterRead = { project, revision: out.revision, content: out.content, conflict: false };
      form.elements.content.value = out.content;
    }
    renderCharterPin();
  }

  function renderCharterPin() {
    const form = byId('project-edit');
    block(form.querySelector('[type=submit]'), !charterRead || charterRead.project !== form.elements.project.value);
    byId('charter-changed').hidden = !charterRead?.conflict;
    byId('charter-revision').textContent = charterRead ? 'Editing revision ' + charterRead.revision + '.' : '';
    byId('project-edit').dataset.revision = charterRead ? charterRead.revision : '';
  }

  // configRead is the project settings form's snapshot: the file's values
  // and the pin of the bytes they were read from, the version a save is
  // pinned to. base holds the text each field showed when the snapshot was
  // taken, so a field is part of the draft once it differs from it.
  // latestError is the loader's error of the newest read, whose file did not
  // load; the form offers no save while it is set. conflict means the file
  // changed since the snapshot while the draft was kept.
  let configRead = null;
  const configRows = new Map();

  // configText is how a setting's value is written in its field. The
  // hearsay_entities mapping is one "entity = Hearsay entity id" line per entry.
  function configText(value) {
    if (value === null || value === undefined) return '';
    if (typeof value === 'object') {
      return Object.entries(value).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0).map(([entity, scope]) => entity + ' = ' + scope).join('\n');
    }
    return String(value);
  }

  // parseEntities reads the hearsay_entities field. bad is the first line
  // that is not "entity = Hearsay entity id".
  function parseEntities(text) {
    const map = {};
    for (const line of text.split('\n')) {
      if (!line.trim()) continue;
      const at = line.indexOf('=');
      const entity = at < 0 ? '' : line.slice(0, at).trim();
      const scope = at < 0 ? '' : line.slice(at + 1).trim();
      if (!entity || !scope) return { map, bad: line.trim() };
      map[entity] = scope;
    }
    return { map, bad: null };
  }

  function configRow(setting) {
    let row = configRows.get(setting.name);
    if (row) return row;
    const sample = setting.effective ?? setting.value;
    const kind = typeof sample === 'number' ? 'number' : typeof sample === 'boolean' ? 'bool' : typeof sample === 'object' && sample !== null ? 'entities' : 'text';
    const input = kind === 'entities' ? el('textarea', { rows: '3', spellcheck: 'false', placeholder: 'entity = Hearsay entity id, one per line' }) :
      kind === 'bool' ? el('select', {}, el('option', { value: 'false' }, 'false'), el('option', { value: 'true' }, 'true')) :
      el('input', { type: kind === 'number' ? 'number' : 'text', step: '1', min: '0' });
    input.name = setting.name;
    row = { name: setting.name, kind, input, marker: el('span', { class: 'marker', 'data-marker': 'default' }),
      pending: el('span', { class: 'marker pending', 'data-marker': 'pending' }), error: el('span', { class: 'field-error', role: 'alert' }) };
    input.addEventListener('input', () => {
      if (configRead?.fieldErrors.delete(setting.name)) renderProjectConfig();
    });
    row.node = el('div', { class: 'setting', 'data-setting': setting.name },
      el('label', {}, el('code', {}, setting.name), input), row.marker, row.pending, row.error);
    configRows.set(setting.name, row);
    return row;
  }

  function settingChanged(read, row) {
    if (row.kind === 'entities') {
      const parsed = parseEntities(row.input.value);
      return parsed.bad !== null || configText(parsed.map) !== read.base.get(row.name);
    }
    return row.input.value.trim() !== read.base.get(row.name);
  }

  function configDirty() {
    const read = configRead;
    return !!read && [...configRows.values()].some(row => settingChanged(read, row));
  }

  // adoptConfig makes out the form's snapshot and shows its file values in
  // the fields: the values in effect, read-only, when the file does not load.
  function adoptConfig(project, out) {
    const base = new Map();
    for (const setting of out.settings) {
      const row = configRow(setting);
      const text = configText(out.error ? setting.effective : setting.value);
      base.set(setting.name, text);
      row.input.value = text;
    }
    configRead = { project, path: out.path, pin: out.pin || '', settings: out.settings, base, conflict: false,
      latestError: out.error || null, fieldErrors: new Map() };
  }

  // receiveConfig takes a read of the project's settings. An untouched form
  // follows it; a form with a draft, or one already marked stale, keeps its
  // fields and pin and notes that the file changed, unless the explicit
  // read asks for the latest values.
  function receiveConfig(project, out, explicit = false) {
    if (!explicit && configRead?.project === project && (configRead.conflict || configDirty())) {
      if ((out.pin || '') !== configRead.pin) configRead.conflict = true;
      else configRead.settings = out.settings;
      configRead.latestError = out.error || null;
    } else {
      adoptConfig(project, out);
    }
    renderProjectConfig();
  }

  function renderProjectConfig() {
    const form = byId('project-config');
    const project = byId('project-edit').elements.project.value;
    const read = configRead?.project === project ? configRead : null;
    const settings = read ? read.settings : [];
    place(byId('config-fields'), settings.map(setting => configRow(setting).node));
    for (const setting of settings) {
      const row = configRow(setting);
      row.input.disabled = !read.pin;
      const file = read.pin ? setting : null;
      row.marker.textContent = file && file.default ? 'Default' + (setting.name === 'capacity.per_workstream' ? ' (inherited from the top-level configuration)' : '') : '';
      row.pending.textContent = file && file.pending ? 'Not yet applied. In effect: ' + (configText(file.effective) || '(none)').replaceAll('\n', '; ') : '';
      const message = read.fieldErrors.get(setting.name) || '';
      row.error.textContent = message;
      if (message) row.input.setAttribute('aria-invalid', 'true');
      else row.input.removeAttribute('aria-invalid');
    }
    byId('config-file').textContent = read ? 'Settings of ' + read.path + (read.pin ? '' : ' (read-only: the file does not load)') :
      (project ? 'Reading the project settings…' : 'Choose a project to manage its settings.');
    const failed = read?.latestError;
    byId('config-load-error').hidden = !failed;
    byId('config-load-error').textContent = failed ? 'The file does not load, so settings cannot be saved until it is fixed by hand: ' + failed.message : '';
    byId('config-changed').hidden = !read?.conflict;
    form.dataset.pin = read ? read.pin : '';
    block(form.querySelector('[type=submit]'), !read || !read.pin || !!read.latestError);
    block(form.querySelector('[data-action=read-config]'), !project);
  }

  // configEdit translates the draft into a save's changes: the fields that
  // differ from the snapshot. Clearing a field that the file sets removes its
  // key; one the file leaves out has nothing to remove. The field error of
  // a value the page cannot send is returned instead.
  function configEdit(read) {
    const edit = { set: {}, clear: [], clear_hearsay_entities: [] };
    for (const setting of read.settings) {
      const row = configRows.get(setting.name);
      if (!row || !settingChanged(read, row)) continue;
      const text = row.input.value.trim();
      if (row.kind === 'entities') {
        const parsed = parseEntities(row.input.value);
        if (parsed.bad !== null) return { field: setting.name, message: setting.name + ': "' + parsed.bad + '" is not "entity = Hearsay entity id"' };
        for (const [entity, scope] of Object.entries(parsed.map)) {
          if (setting.value[entity] !== scope) (edit.set.hearsay_entities ||= {})[entity] = scope;
        }
        for (const entity of Object.keys(setting.value)) {
          if (!(entity in parsed.map)) edit.clear_hearsay_entities.push(entity);
        }
      } else if (text === '') {
        if (!setting.default) edit.clear.push(setting.name);
      } else if (row.kind === 'bool') {
        edit.set[setting.name] = text === 'true';
      } else if (row.kind === 'number') {
        if (!/^\d+$/.test(text)) return { field: setting.name, message: setting.name + ': must be a whole number' };
        edit.set[setting.name] = Number(text);
      } else {
        edit.set[setting.name] = text;
      }
    }
    return edit;
  }

  function saveConfig(event) {
    event.preventDefault();
    const form = byId('project-config');
    const read = configRead;
    if (!read || read.project !== byId('project-edit').elements.project.value || !read.pin || read.latestError) return;
    const result = form.querySelector('.result');
    read.fieldErrors.clear();
    const edit = configEdit(read);
    if (edit.field) {
      read.fieldErrors.set(edit.field, edit.message);
      renderProjectConfig();
      show(result, 'error', edit.message);
      return;
    }
    if (!Object.keys(edit.set).length && !edit.clear.length && !edit.clear_hearsay_entities.length) {
      adoptConfig(read.project, { path: read.path, pin: read.pin, settings: read.settings });
      configRead.conflict = read.conflict;
      renderProjectConfig();
      show(result, 'ok', 'No settings changed.');
      return;
    }
    act(result, event.submitter, () => request('PUT', '/projects/config/' + read.project, { pin: read.pin, ...edit }), out => {
      if (configRead === read) {
        adoptConfig(read.project, out);
        renderProjectConfig();
      }
      mark(['status', 'config']);
      if (out.outcome === 'saved_not_applied') {
        return { outcome: 'saved-not-applied', text: 'Saved to ' + out.path + ' but not applied: ' + out.reason +
          ' The saved values are shown as not yet applied; Reload in Configuration applies them.' };
      }
      return 'Saved project settings to ' + out.path + '.';
    }, err => {
      if (configRead !== read) return;
      if (err.code === 'conflict') read.conflict = true;
      const row = err.field && (configRows.get(err.field) || [...configRows.values()].find(r => err.field.startsWith(r.name + '.')));
      if (row) read.fieldErrors.set(row.name, err.message);
      renderProjectConfig();
    });
  }

  function selectManagedProject() {
    const form = byId('project-edit');
    const project = form.elements.project.value;
    if (charterProject === project) return;
    charterProject = project;
    charterRead = null;
    form.elements.content.value = '';
    show(form.querySelector('.result'), '', '');
    for (const [id, row] of managementRepos) {
      if (project || !row.retain) managementRepos.delete(id);
    }
    configRead = null;
    show(byId('project-config').querySelector('.result'), '', '');
    for (const name of Object.keys(failures)) {
      if (isManagementView(name)) delete failures[name];
    }
    renderCharterPin();
    renderProjectConfig();
    if (project) mark(['projects/charter/' + project, 'projects/config/' + project]);
  }

  function renderProjectManagement() {
    selectManagedProject();
    const project = byId('project-edit').elements.project.value;
    const configured = projects().find(p => p.id === project);
    const state = configured?.charter_state;
    byId('charter-state').textContent = state ? state.rules + (state.rules === 1 ? ' rule' : ' rules') + ', revision ' + state.revision + '.' :
      (project ? 'Charter state not reported.' : 'Choose a project to manage its charter and repos.');
    block(byId('project-remove'), !configured);
    renderProjectConfig();
    const repos = configured?.repos || [];
    const active = new Set(repos.map(r => r.id));
    for (const [id, row] of managementRepos) {
      if (!active.has(id) && !row.retain) managementRepos.delete(id);
    }
    for (const repo of repos) {
      let row = managementRepos.get(repo.id);
      if (!row) {
        row = { title: el('h4'), result: el('p', {class: 'result', role: 'status'}), buttons: [] };
        for (const [action, label] of [['rebase', 'Rebase workstreams'], ['remove', 'Remove repo from active work']]) {
          const button = el('button', {type: 'button', 'data-action': action}, label);
          button.addEventListener('click', () => {
            if (action === 'remove') row.retain = true;
            act(row.result, button, () => request(action === 'remove' ? 'DELETE' : 'POST',
              action === 'remove' ? '/projects' : '/projects/' + action, {project: repo.id}), out => {
              if (action === 'remove') {
                row.buttons.forEach(b => block(b, true));
              }
              mark(['status', 'config']);
              return action === 'remove' ? 'Repo removed from active work; its trace and clone are retained.' :
                'Workstream rebase: ' + out.covered.length + ' requested, ' + out.skipped.length + ' skipped.' +
                  out.skipped.map(skip => ' ' + skip.workstream + ': ' + skip.reason + '.').join('');
            }, () => { row.retain = false; });
          });
          row.buttons.push(button);
        }
        row.node = el('article', {class: 'managed-repo', 'data-managed-repo': repo.id}, row.title,
          el('div', {class: 'actions'}, ...row.buttons), row.result);
        managementRepos.set(repo.id, row);
      }
      row.title.textContent = repoLabel(repo.id);
    }
    place(byId('project-repos'), [...managementRepos.values()].map(row => row.node));
  }
  let draftRead = null;
  let baseRead = null;
  let handinRetry = null;
  let handinProjects = [];
  function selectedHandinProjects() {
    const selected = [...byId('handin-form').elements.project.selectedOptions].map(o => o.value);
    handinProjects = [...handinProjects.filter(id => selected.includes(id)), ...selected.filter(id => !handinProjects.includes(id))];
    return handinProjects;
  }

  // formAction runs an owner action a form submits. With opts.reset, a
  // successful submission clears the form back to its default values, the
  // way the browser's own reset does, and recomputes what renderOwnerForms
  // keeps current, such as a hidden workstream field; a failed submission
  // leaves the form exactly as the owner left it.
  function formAction(form, button, operation, done, opts) {
    const origin = captureOutcome(form.querySelector('.result'));
    return act(form.querySelector('.result'), button, operation, (out) => {
      mark(['status', 'config', 'inbox', 'charter']);
      const text = done(out);
      if (opts && opts.reset && (!origin.scope || workstreamForms.includes(form.id) || origin.key === origin.scope.instance())) {
        form.reset();
        renderOwnerForms();
      }
      return text;
    });
  }

  // workstreamForms act on the workstream the main area shows.
  const workstreamForms = ['documents-form', 'base-form', 'workstream-action', 'trace-form'];

  // clearWorkstreamForms empties what the workstream's forms read of the
  // workstream shown before.
  function clearWorkstreamForms() {
    draftRead = null;
    baseRead = null;
    const documents = byId('documents-form');
    documents.elements.spec.value = '';
    documents.elements.plan.value = '';
    byId('document-revisions').textContent = '';
    byId('trace-content').replaceChildren();
    renderOutcomes();
  }

  function renderOwnerForms() {
    const options = [['', 'Choose a project'], ...projects().map(p => [p.id, projectName(p.id)])];
    for (const select of document.querySelectorAll('[data-project-select]')) {
      setOptions(select, select.multiple ? options.filter(([id]) => id) : options);
    }
    for (const select of document.querySelectorAll('[data-repo-select]')) {
      setOptions(select, [['', 'Choose a repo'], ...configuredRepos().map(r => [r.id, r.label])]);
    }
    const all = streams();
    const handin = byId('handin-form');
    const handinSelected = selectedHandinProjects();
    setOptions(handin.elements.base, [['', 'Project upstream'], ...all.filter(w => handinSelected.includes(w.project) && w.state !== 'abandoned' && !w.archived).map(w => [w.workstream, goal(w)])]);
    byId('handin-project-order').textContent = handinProjects.length ? 'Selected order: ' + handinProjects.map(projectName).join(' → ') + '. First project hosts the shared chief and conversation.' : 'Choose projects in order; the first hosts the shared chief and conversation.';
    const w = shown();
    for (const id of workstreamForms) {
      byId(id).elements.workstream.value = w ? w.workstream : '';
    }
    const charter = byId('project-edit');
    block(charter.querySelector('[type=submit]'), !charterRead || charterRead.project !== charter.elements.project.value);
    const form = byId('documents-form');
    block(form.querySelector('[type=submit]'), !draftRead || !w || draftRead.workstream !== w.workstream || !['sketched', 'in-shed'].includes(w.state));
    const baseForm = byId('base-form');
    setOptions(baseForm.elements.base, [['', 'Project upstream'], ...all.filter(b => w && b.project === w.project && b.workstream !== w.workstream && b.state !== 'abandoned' && !b.archived).map(b => [b.workstream, goal(b)])]);
    const action = byId('workstream-action');
    action.querySelector('[data-field=archive-once]').hidden = action.elements.action.value !== 'abandon';
    block(baseForm.querySelector('[type=submit]'), !w || !baseRead || baseRead.workstream !== w.workstream || !['handed', 'sketched', 'in-shed'].includes(w.state));
  }

  function readDocuments(button) {
    const documents = byId('documents-form');
    const workstream = documents.elements.workstream.value;
    if (!workstream) { return; }
    formAction(documents, button, () => request('GET', '/documents/' + workstream), out => {
      if (documents.elements.workstream.value === workstream) {
        documents.elements.spec.value = out.spec ? out.spec.content : '';
        documents.elements.plan.value = out.plan ? out.plan.content : '';
        draftRead = out.spec && out.plan ? {workstream, spec_revision: out.spec.revision, plan_revision: out.plan.revision} : null;
        byId('document-revisions').textContent = draftRead ? 'Spec revision ' + out.spec.revision + '; plan revision ' + out.plan.revision + '.' : 'The architect has not drafted both documents yet.';
      }
      renderOwnerForms();
      return 'Documents loaded. Drafts can be edited before ratification; sealed documents require an amendment.';
    });
  }

  function readBase(button) {
    const baseForm = byId('base-form');
    const workstream = baseForm.elements.workstream.value;
    if (!workstream) { return; }
    formAction(baseForm, button, () => request('GET', '/base/' + workstream), out => {
      if (baseForm.elements.workstream.value === workstream) { baseRead = out; renderOwnerForms(); baseForm.elements.base.value = out.base || ''; }
      return 'Dependency revision ' + out.revision + ' loaded.';
    });
  }

  // readWorkstreamDocuments reads the documents and dependency of the
  // workstream shown, as the Documents tab opens on it.
  function readWorkstreamDocuments() {
    renderOwnerForms();
    readDocuments(byId('documents-form').querySelector('[data-action=read]'));
    readBase(byId('base-form').querySelector('[data-action=read]'));
  }

  function setupOwnerForms() {
    const add = byId('project-add');
    add.addEventListener('submit', event => {
      event.preventDefault();
      const body = Object.fromEntries(new FormData(add));
      formAction(add, event.submitter, () => request('POST', '/projects', body), out => 'Registered ' + out.project.name + '. Read and write its charter in Project management before handing in work.', { reset: true });
    });
    const handin = byId('handin-form');
    handin.elements.project.addEventListener('change', renderOwnerForms);
    handin.addEventListener('submit', event => {
      event.preventDefault();
      const body = {projects: selectedHandinProjects(), base: handin.elements.base.value, skip_debate: handin.elements.skip_debate.checked};
      body[handin.elements.source.value === 'url' ? 'url' : 'stdin'] = handin.elements.content.value;
      const input = JSON.stringify(body);
      if (!handinRetry || handinRetry.input !== input) {
        handinRetry = {input, key: 'web-' + Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('')};
      }
      body.key = handinRetry.key;
      formAction(handin, event.submitter, () => request('POST', '/handin', body), out => {
        handinRetry = null;
        select(out.workstream);
        return 'Handed in ' + out.workstream + '.';
      }, { reset: true });
    });
    const charter = byId('project-edit');
    charter.elements.project.addEventListener('change', () => {
      selectManagedProject();
      show(byId('project-result'), '', '');
      renderOwnerForms();
      renderProjectManagement();
    });
    charter.querySelector('[data-action=read-charter]').addEventListener('click', event => {
      selectManagedProject();
      const project = charter.elements.project.value;
      if (!project) { show(charter.querySelector('.result'), 'error', 'Choose a project.'); return; }
      act(charter.querySelector('.result'), event.target, () => request('GET', '/projects/charter/' + project), out => {
        if (charter.elements.project.value === project) receiveCharter(project, out, true);
        renderOwnerForms();
        return 'Read charter revision ' + out.revision + '.';
      });
    });
    charter.addEventListener('submit', event => {
      event.preventDefault();
      if (!charterRead || charterRead.project !== charter.elements.project.value) { return; }
      const pin = charterRead;
      const content = charter.elements.content.value;
      act(charter.querySelector('.result'), event.submitter, () => request('PUT', '/projects/charter/' + pin.project,
        {revision: pin.revision, content}), out => {
          if (charterRead === pin) {
            charterRead = {project: pin.project, revision: out.revision, content, conflict: false};
            renderCharterPin();
          }
          renderOwnerForms();
          mark(['status', 'config', 'charter']);
          return 'Saved charter revision ' + out.revision + '.';
        }, err => {
          // Keep a stale draft pinned until the owner explicitly rereads.
          if (charterRead === pin && err.code === 'conflict') { pin.conflict = true; renderCharterPin(); }
        });
    });
    const config = byId('project-config');
    config.addEventListener('submit', saveConfig);
    config.querySelector('[data-action=read-config]').addEventListener('click', event => {
      selectManagedProject();
      const project = charter.elements.project.value;
      if (!project) { show(config.querySelector('.result'), 'error', 'Choose a project.'); return; }
      act(config.querySelector('.result'), event.target, () => request('GET', '/projects/config/' + project), out => {
        if (charter.elements.project.value === project) {
          adoptConfig(project, out);
          renderProjectConfig();
        }
        return out.error ? { outcome: 'error', text: out.error.message } : 'Read the settings of ' + out.path + '.';
      });
    });
    byId('project-remove').addEventListener('click', event => {
      const project = charter.elements.project.value;
      if (!project) return;
      act(byId('project-result'), event.target, () => request('DELETE', '/projects', {project}), () => {
        mark(['status', 'config']);
        return 'Project removed from active work; its trace and clone are retained.';
      });
    });
    const documents = byId('documents-form');
    documents.querySelector('[data-action=read]').addEventListener('click', event => readDocuments(event.target));
    documents.addEventListener('submit', event => {
      event.preventDefault();
      if (!draftRead || draftRead.workstream !== documents.elements.workstream.value) { return; }
      const pin = draftRead;
      formAction(documents, event.submitter, () => request('PUT', '/documents/' + pin.workstream,
        {spec_revision: pin.spec_revision, plan_revision: pin.plan_revision, spec: documents.elements.spec.value, plan: documents.elements.plan.value}), out => {
          if (draftRead === pin) {
            draftRead = {workstream: pin.workstream, spec_revision: out.spec.revision, plan_revision: out.plan.revision};
            byId('document-revisions').textContent = 'Spec revision ' + out.spec.revision + '; plan revision ' + out.plan.revision + '.';
          }
          return 'Draft documents saved for debate.';
        });
    });
    const baseForm = byId('base-form');
    baseForm.querySelector('[data-action=read]').addEventListener('click', event => readBase(event.target));
    baseForm.addEventListener('submit', event => {
      event.preventDefault();
      if (!baseRead || baseRead.workstream !== baseForm.elements.workstream.value) { return; }
      const pin = baseRead;
      formAction(baseForm, event.submitter, () => request('PUT', '/base/' + pin.workstream, {base: baseForm.elements.base.value, revision: pin.revision}), out => {
        if (baseRead === pin) { baseRead = out; }
        return 'Dependency revision ' + out.revision + ' saved.';
      });
    });
    const action = byId('workstream-action');
    action.addEventListener('submit', event => {
      event.preventDefault();
      const kind = action.elements.action.value;
      const workstream = action.elements.workstream.value;
      const note = action.elements.note.value.trim();
      const bodies = {object: {argument: note}, more: {rounds: 1}, redraft: {note}, skip: {}};
      const archive = kind === 'abandon' && action.elements.archive.checked;
      formAction(action, event.submitter, async () => {
        if (kind === 'abandon') {
          return abandonWorkstream(workstream, note, archive);
        }
        return request('POST', '/shed/' + kind + '/' + workstream, bodies[kind]);
      }, () => {
        if (!archive) {
          return 'Workstream action recorded.';
        }
        leaveArchived(workstream);
        return 'Workstream abandoned and archived.';
      }, { reset: true });
    });
    action.elements.action.addEventListener('change', renderOwnerForms);
    const trace = byId('trace-form');
    trace.addEventListener('submit', event => {
      event.preventDefault();
      const kind = trace.elements.kind.value;
      const selector = trace.elements.selector.value.trim();
      if (kind && !selector) { show(trace.querySelector('.result'), 'error', 'Name the unit, criterion, or commit.'); return; }
      const workstream = trace.elements.workstream.value;
      const path = '/trace/' + workstream + (kind ? '/' + kind + '/' + encodeURIComponent(selector) : '');
      formAction(trace, event.submitter, () => request('GET', path), out => {
        if (trace.elements.workstream.value === workstream) byId('trace-content').replaceChildren(traceTree(out));
        return 'Trace loaded.';
      });
    });
  }

  // setupSessionForms wires the exploratory-session form and the session view's
  // chat, stop, commit, check, push and end controls. Each control's action
  // runs against the session the view shows, read again at the moment it is
  // used, since only one session is shown at a time.
  function setupSessionForms() {
    const add = byId('session-add-form');
    add.addEventListener('submit', (event) => {
      event.preventDefault();
      const body = { project: add.elements.project.value, profile: add.elements.profile.value };
      const title = add.elements.title.value.trim();
      if (title !== '') {
        body.title = title;
      }
      formAction(add, event.submitter, () => request('POST', '/sessions', body), (out) => {
        sessionIds.add(out.session.workstream);
        select(out.session.workstream);
        return 'Opened session ' + out.session.workstream + '.';
      }, { reset: true });
    });
    const form = byId('session-form');
    setupDictation(form, byId('session-result'), () => ui.view === 'session' ? shownSession()?.workstream : null);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      const w = shownSession();
      if (w) {
        sendSession(w.workstream);
      }
    });
    // Enter sends and Shift+Enter starts a new line where there is a
    // keyboard; on a touch screen Enter starts a new line.
    form.elements.text.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && window.matchMedia('(pointer: fine)').matches) {
        event.preventDefault();
        form.requestSubmit();
      }
    });
    byId('session-stop').addEventListener('click', () => {
      const w = shownSession();
      if (w) {
        stopSession(w.workstream);
      }
    });
    byId('session-check').addEventListener('click', () => {
      const w = shownSession();
      if (w) {
        checkSession(w.workstream);
      }
    });
    byId('session-push').addEventListener('click', () => {
      const w = shownSession();
      if (w) {
        pushSession(w.workstream);
      }
    });
    byId('session-end').addEventListener('click', () => {
      const w = shownSession();
      if (w) {
        endSession(w.workstream);
      }
    });
    byId('session-destroy').addEventListener('click', () => {
      const w = shownSession();
      if (w) destroyConversation(w.workstream, false);
    });
    byId('session-commit-form').addEventListener('submit', (event) => {
      event.preventDefault();
      const w = shownSession();
      if (w) {
        commitSession(w.workstream);
      }
    });
  }

  // setupDesignForms wires design creation, chat, workgroup start and destroy.
  function setupDesignForms() {
    const add = byId('design-add-form');
    // A new-project session lives in the chosen project or, with none chosen,
    // in the first active project, so the project choice is optional then.
    const syncNewProject = () => {
      add.elements.project.required = !add.elements.new_project.checked;
    };
    add.elements.new_project.addEventListener('change', syncNewProject);
    add.addEventListener('reset', () => setTimeout(syncNewProject));
    add.addEventListener('submit', (event) => {
      event.preventDefault();
      const newProject = add.elements.new_project.checked;
      formAction(add, event.submitter, () => request('POST', '/design', { project: add.elements.project.value, new_project: newProject }), (out) => {
        designIds.set(out.design.workstream, out.design);
        select(out.design.workstream);
        return 'Opened design session ' + out.design.workstream + '.';
      }, { reset: true });
    });
    const form = byId('design-form');
    setupDictation(form, byId('design-result'), () => ui.view === 'design' ? shownDesign()?.workstream : null);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      const w = shownDesign();
      if (w) {
        sendDesign(w.workstream);
      }
    });
    form.elements.text.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && window.matchMedia('(pointer: fine)').matches) {
        event.preventDefault();
        form.requestSubmit();
      }
    });
    byId('design-start').addEventListener('click', () => {
      const w = shownDesign();
      if (w) {
        startDesign(w.workstream);
      }
    });
    byId('design-destroy').addEventListener('click', () => {
      const w = shownDesign();
      if (w) destroyConversation(w.workstream, true);
    });
  }

  function traceTree(value) {
    if (value === null || typeof value !== 'object') {
      return el('span', {class: 'text'}, value === null ? 'None' : String(value));
    }
    if (Array.isArray(value)) {
      return el('ol', {}, ...value.map(item => el('li', {}, traceTree(item))));
    }
    return el('dl', {}, ...Object.entries(value).flatMap(([name, content]) => [
      el('dt', {}, name.replaceAll('_', ' ')), el('dd', {}, traceTree(content))]));
  }

  // shownFeed names only the visible conversation, and waits for its read
  // before treating a placeholder as a rendered feed.
  function shownFeed() {
    let key, box;
    if (ui.view === 'workstream' && ui.tab === 'conversation' && shown()) {
      key = 'feed/' + shown().workstream;
      box = byId('workstreams').querySelector('.feed');
    } else if (ui.view === 'workstream' && ui.tab === 'activity' && shown()) {
      key = 'activity/' + shown().workstream;
      box = byId('workstream-activity').querySelector('.activity-log');
      return box ? { key, box, ready: !!views['feed/' + shown().workstream], end: box } : null;
    } else if (ui.view === 'workgroup') {
      key = groupFeed(ui.workgroup);
      box = byId('workgroup-content').querySelector('.feed');
    } else if (ui.view === 'session' && shownSession()) {
      key = 'feed/' + shownSession().workstream;
      box = byId('session-feed');
    } else if (ui.view === 'design' && shownDesign()) {
      key = 'design/' + shownDesign().workstream + '/conversation';
      box = byId('design-feed');
    }
    return box ? { key, box, ready: !!views[key] } : null;
  }

  let renderedFeed = null;
  let feedSnap = 0;

  // Count the displayed entries, including session echoes merged by turn.
  // Updating an entry in place does not count as a new message. Scrolling
  // after layout also lets a send's result finish painting beside its echo.
  function snapFeed() {
    const feed = shownFeed();
    if (!feed) return;
    const count = feed.box.querySelectorAll(':scope > ol > li').length;
    if (!feed.ready && count === 0) return;
    const pinned = ui.tab === 'activity' && ui.view === 'workstream' && ui.focus !== '';
    const snap = !pinned && (ui.toEnd || renderedFeed?.key !== feed.key || count > renderedFeed.count);
    renderedFeed = { key: feed.key, count };
    if (!snap) return;
    ui.toEnd = false;
    const pending = ++feedSnap;
    requestAnimationFrame(() => {
      const current = shownFeed();
      if (pending !== feedSnap || current?.key !== feed.key) return;
      const main = byId('main');
      // Session controls follow the chat; land at the conversation's end
      // with its composer visible, leaving those controls below it.
      const end = feed.end || feed.box.closest('.thread');
      main.scrollTop += end.getBoundingClientRect().bottom - main.getBoundingClientRect().bottom;
    });
  }

  function render() {
    renderOutcomes();
    forget();
    pruneInbox();
    markSeen();
    renderViews();
    renderOwnerForms();
    renderProjectManagement();
    renderProblems();
    renderHeader();
    renderSidebar();
    renderProjectPane();
    renderWorkstreamView();
    renderWorkgroupView();
    renderSessionView();
    renderSessionAddForm();
    renderDesignView();
    renderPauses();
    renderPauseForm();
    renderActivity();
    renderCapacity();
    renderPriority();
    renderProfiles();
    renderProviders();
    renderConfig();
    renderDictation();
    focusEntry();
    snapFeed();
  }

  setupOutcomeScopes();
  setupLayout();
  setupEngine();
  setupOwnerForms();
  setupSessionForms();
  setupDesignForms();
  window.addEventListener('pagehide', () => {
    for (const composer of dictationComposers.values()) composer.stop();
  });
  render();
  byId('pause-form').addEventListener('submit', pause);
  byId('priority-form').addEventListener('submit', setPriority);
  byId('priority-project').addEventListener('change', () => {
    byId('priority-result').textContent = '';
    renderPriority();
  });
  byId('priority-clear').addEventListener('click', clearPriority);
  byId('reload').addEventListener('click', reload);
  setInterval(() => {
    if (document.visibilityState === 'visible' && document.body.dataset.connection === 'live') {
      mark(['config']);
    }
  }, configInterval);
  setInterval(expireDismissals, dismissCheckInterval);
  // Whether an activity entry counts as blocked depends on the clock, not
  // only on the last read, so it is kept current on the same tick.
  setInterval(() => {
    renderHeader();
    renderActivity();
  }, dismissCheckInterval);
  window.addEventListener('popstate', restoreRoute);
  window.addEventListener('online', reconnectNow);
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      reconnectNow();
      mark(['config']);
      render();
    }
  });
  connect();
})();
