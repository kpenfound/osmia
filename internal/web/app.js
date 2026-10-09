'use strict';

(() => {
  const api = '/v1';

  // The views each event kind names. "feed" is the feed of the event's
  // workstream and "feeds" that of every workstream the page shows. Kinds the
  // page does not show are ignored; every stream starts with a resync.
  const reads = {
    resync: ['status', 'runtime', 'config', 'inbox', 'charter', 'feeds'],
    workstream: ['status', 'feed'],
    conversation: ['feed'],
    inbox: ['inbox', 'charter'],
    runtime: ['runtime', 'status'],
    config: ['config'],
    spend: ['status'],
  };

  // An edit of a configuration file on disk is not announced, so the page
  // reads /config again this often while it is visible and live.
  const configInterval = 10000;

  // How often the page checks whether a dismissed feed action item has
  // passed its DISMISS_TIMEOUT_MS comeback window. This runs independently
  // of any other activity, so a dismissal still clears on time even if
  // nothing else prompts a render.
  const dismissCheckInterval = 1000;

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

  // ui is what the owner is looking at: the selected workstream, the view
  // the main area shows and the workstream's tab. seen holds the activity the
  // owner last saw on each workstream. initial holds the workstreams the
  // first status read listed, whose current activity counts as seen the
  // first time the page shows them.
  const ui = {
    selected: stored('selected', null),
    view: 'workstream',
    tab: 'feed',
    seen: stored('seen', {}),
    initial: null,
    toEnd: true,
  };

  // el builds an element; strings among the children become text nodes.
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

  // request calls the API and returns its answer, or throws an error with
  // the API's message when it refuses.
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
      throw new Error(refusal);
    }
    return out;
  }

  // listedFeeds names the feeds the page reads: those of the workstreams in
  // the list of work. Archived workstreams have no readable history.
  function listedFeeds() {
    return views.status ? views.status.workstreams.filter((w) => !w.archived).map((w) => 'feed/' + w.workstream) : [];
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
            views[name] = await request('GET', '/' + name);
            delete failures[name];
          } catch (err) {
            failures[name] = 'Cannot read /' + name + ': ' + err.message;
          }
        }));
        if (views.status) {
          const listed = new Set(listedFeeds());
          for (const name of [...Object.keys(views), ...Object.keys(failures)]) {
            if (name.startsWith('feed/') && !listed.has(name)) {
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

  // expand turns an event's view names into view paths.
  function expand(name, event) {
    if (name === 'feeds') {
      return listedFeeds();
    }
    if (name === 'feed') {
      let data = {};
      try {
        data = JSON.parse(event.data);
      } catch {
        return [];
      }
      return data.workstream ? ['feed/' + data.workstream] : [];
    }
    return [name];
  }

  function setConnection(state) {
    document.body.dataset.connection = state;
    const label = { connecting: 'Connecting…', live: 'Live', lost: 'Reconnecting…' }[state];
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
      stream.addEventListener(kind, (event) => mark(names.flatMap((name) => expand(name, event))));
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

  // show writes an action's outcome: pending, ok or error.
  function show(result, outcome, text) {
    result.dataset.outcome = outcome;
    result.textContent = text;
  }

  // act runs one owner action with its button disabled and shows what the
  // API answered in result: done's text on success, the refusal otherwise,
  // after which refused runs, when given. Once the action ends, the button
  // is enabled again unless a render has blocked it meanwhile.
  async function act(result, button, call, done, refused) {
    button.dataset.busy = 'true';
    button.disabled = true;
    show(result, 'pending', 'Sending…');
    try {
      show(result, 'ok', done(await call()));
    } catch (err) {
      show(result, 'error', err.message);
      if (refused) {
        refused();
      }
    } finally {
      delete button.dataset.busy;
      button.disabled = button.dataset.blocked === 'true';
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
    const chosen = select.value;
    select.dataset.options = key;
    select.replaceChildren(...options.map(([value, label]) => el('option', { value }, label)));
    if (options.some(([value]) => value === chosen)) {
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
        return 'Project ' + target.project;
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

  function projectName(id) {
    const p = projects().find((p) => p.id === id);
    return p && p.name ? p.name : id;
  }

  function goal(w) {
    return w.status ? w.status.goal : w.workstream;
  }

  function renderProblems() {
    const items = Object.values(failures);
    for (const name of ['status', 'runtime']) {
      const view = views[name];
      if (view && Array.isArray(view.diagnostics)) {
        for (const d of view.diagnostics) {
          items.push(d.field + ': ' + d.message);
        }
      }
    }
    const list = byId('problem-list');
    list.replaceChildren(...items.map((text) => el('li', {}, text)));
    byId('problems').hidden = items.length === 0;
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
  // active projects and each project's workstreams.
  function pauseTargets() {
    const targets = [['factory', 'The factory', { scope: 'factory' }]];
    for (const p of projects()) {
      targets.push(['project:' + p.id, 'Project ' + projectName(p.id), { scope: 'project', project: p.id }]);
      for (const w of views.status ? views.status.workstreams.filter((w) => w.project === p.id && !w.archived) : []) {
        targets.push(['workstream:' + w.workstream, projectName(p.id) + ': ' + goal(w),
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
          el('span', { 'data-field': 'slots' }, r.used + ' / ' + r.limit + ' slots')),
        r.waiting.length === 0
          ? el('div', { class: 'meta' }, 'Nothing waits.')
          : el('ul', { class: 'waiting' }, ...r.waiting.map((w) => el('li', { 'data-field': 'waiting' },
            el('span', { class: 'id' }, w.workstream),
            ' ', w.unit ? 'unit ' + w.unit : 'agent ' + w.agent,
            ': ', waitReasons[w.reason] || w.reason))))),
      el('p', { class: 'meta', 'data-field': 'per-workstream' }, 'Each workstream may hold ' + capacity.per_workstream + ' slots.'));
  }

  function streams() {
    return views.status ? views.status.workstreams : [];
  }

  // shown is the workstream the main area shows: the selected one while the
  // status lists it as unarchived, and otherwise the first in the list of
  // work, which the status already orders by last activity.
  function shown() {
    const list = streams();
    return list.find((w) => !w.archived && w.workstream === ui.selected) || list.find((w) => !w.archived) || null;
  }

  function terminal(w) {
    return w.state === 'delivered' || w.state === 'abandoned';
  }

  // inboxOf lists the workstream's open entries, less any a submission has
  // dismissed from the feed; see dismissed above.
  function inboxOf(id) {
    return views.inbox ? views.inbox.entries.filter((e) => e.workstream === id && !dismissed.has(decisionKey(e))) : [];
  }

  function proposalsOf(id) {
    return views.charter ? views.charter.proposals.filter((p) => p.workstream === id) : [];
  }

  // activity is what the owner has seen of a workstream once they look at
  // it: its state, status and units, what waits on them and its
  // conversation with the chief of staff. Sessions alone are not activity.
  // It is null until the feed and inbox are read.
  function activity(w) {
    const feed = views['feed/' + w.workstream];
    if (!feed || !views.inbox) {
      return null;
    }
    const conversation = feed.entries.filter((e) => conversationKinds.has(e.kind));
    const last = conversation[conversation.length - 1];
    return JSON.stringify([
      w.state,
      w.status ? [w.status.goal, w.status.attention, w.status.note] : null,
      (w.units || []).map((u) => u.unit + ':' + u.state).sort(),
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
    const ids = new Set(streams().map((w) => w.workstream));
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

  function row(id) {
    let r = rows.get(id);
    if (r) {
      return r;
    }
    r = {
      title: el('span', { class: 'title', 'data-field': 'goal' }),
      project: el('span', { 'data-field': 'project' }),
      state: el('span', { 'data-field': 'state' }),
      signals: el('span', { class: 'signals' }),
    };
    r.button = el('button', { type: 'button', class: 'row', 'data-select': id },
      r.title, el('span', { class: 'sub' }, r.project, ' · ', r.state), r.signals);
    r.button.addEventListener('click', () => select(id));
    r.node = el('li', {}, r.button);
    rows.set(id, r);
    return r;
  }

  function renderSidebar() {
    const list = byId('workstream-list');
    if (!views.status) {
      place(list, [el('li', { class: 'meta none', 'data-field': 'loading' }, 'Loading workstreams…')]);
      return;
    }
    const ids = new Set(streams().map((w) => w.workstream));
    for (const id of rows.keys()) {
      if (!ids.has(id)) {
        rows.delete(id);
      }
    }
    const current = shown();
    const archived = streams().filter((w) => w.archived);
    const group = byId('archived');
    group.hidden = archived.length === 0;
    byId('archived-count').textContent = String(archived.length);
    const entry = (w) => {
      const r = row(w.workstream);
      const needs = inboxOf(w.workstream).length + proposalsOf(w.workstream).length;
      const paused = pausedIn(w).length > 0;
      r.title.textContent = goal(w);
      r.title.title = goal(w);
      r.project.textContent = projectName(w.project);
      r.state.textContent = w.archived ? (w.trace_deleted ? 'archived · trace deleted' : 'archived · cleanup pending') : (w.state || 'handed');
      r.button.disabled = !!w.archived;
      r.button.title = w.archived ? 'Permanently archived; history is unavailable.' : '';
      r.button.setAttribute('aria-current', String(current !== null && current.workstream === w.workstream));
      r.signals.replaceChildren(...[
        paused ? el('span', { class: 'paused', 'data-field': 'paused', title: 'Paused' }, 'paused') : null,
        w.agents && w.agents.length > 0 ? el('span', { class: 'working', 'data-field': 'working', title: w.agents.length + ' running' }) : null,
        needs > 0 ? el('span', { class: 'needs', 'data-field': 'needs', title: needs + ' waiting for you' }, String(needs)) : null,
        needs === 0 && unread(w) ? el('span', { class: 'unread', 'data-field': 'unread', title: 'New activity' }) : null,
      ].filter((node) => node !== null));
      return r.node;
    };
    place(byId('archived-list'), archived.map(entry));
    const work = streams().filter((w) => !w.archived);
    if (work.length === 0) {
      list.replaceChildren(el('li', { class: 'meta none' }, archived.length === 0 ? 'No workstreams yet.' : 'Every workstream is archived.'));
      return;
    }
    place(list, work.map(entry));
  }

  // setPicker opens or closes the workstream list at phone widths, where it
  // covers the page until the owner picks a workstream or closes it.
  function setPicker(open) {
    document.body.dataset.picker = open ? 'open' : 'closed';
    byId('picker-toggle').setAttribute('aria-expanded', String(open));
  }

  // select shows a workstream in the main area and marks it as the current
  // selection, without changing the sidebar order.
  function select(id) {
    const changed = ui.selected !== id;
    ui.selected = id;
    store('selected', id);
    setPicker(false);
    openView('workstream');
    if (changed) {
      clearWorkstreamForms();
      ui.toEnd = true;
    }
    if (streams().some((w) => w.workstream === id && w.archived)) {
      byId('archived').open = true;
    }
    render();
    if (changed && !views['feed/' + id]) {
      mark(['feed/' + id]);
    }
    if (changed && ui.tab === 'documents') {
      readWorkstreamDocuments();
    }
  }

  // openView shows a view in the main area: the selected workstream, or one
  // of the views the header opens.
  function openView(view) {
    for (const popover of document.querySelectorAll('[popover]')) {
      if (popover.matches(':popover-open')) {
        popover.hidePopover();
      }
    }
    if (view === 'handin') {
      const w = shown();
      const project = byId('handin-form').elements.project;
      if (w && project.value === '') {
        project.value = w.project;
      }
    }
    const changed = ui.view !== view;
    ui.view = view;
    setPicker(false);
    render();
    if (changed) {
      byId('main').scrollTop = view === 'workstream' ? byId('main').scrollHeight : 0;
    }
  }

  // beekeeperAuthorLabel names who sent one Beekeeper chat message: the
  // owner, the Beekeeper itself, a named workstream's chief of staff
  // relaying a reply, or a failure note.
  function beekeeperAuthorLabel(author) {
    switch (author.kind) {
      case 'owner':
        return 'You';
      case 'beekeeper':
        return 'Beekeeper';
      case 'chief_of_staff':
        return 'Chief of staff: ' + workstreamName(author.workstream);
      case 'failure':
        return 'Failed';
      default:
        return author.kind;
    }
  }

  let beekeeperShown = null;

  // renderBeekeeperFeed shows the Beekeeper chat's recent messages, oldest
  // first, with each one's author. It reads only what views.beekeeper
  // already holds: the page never asks for more than the recent window and
  // offers no way to load older history.
  function renderBeekeeperFeed() {
    const view = views.beekeeper;
    const key = view ? JSON.stringify(view.messages) : (failures.beekeeper ? 'failed' : 'reading');
    if (beekeeperShown === key) {
      return;
    }
    beekeeperShown = key;
    const box = byId('beekeeper-feed');
    if (!view) {
      box.replaceChildren(el('p', { class: 'meta' }, failures.beekeeper ? 'The beekeeper chat is unavailable.' : 'Reading the beekeeper chat…'));
    } else if (view.messages.length === 0) {
      box.replaceChildren(el('p', { class: 'meta' }, 'Nothing has happened yet.'));
    } else {
      box.replaceChildren(el('ol', {}, ...view.messages.map((m) => el('li', { class: 'entry', 'data-kind': m.author.kind },
        el('div', { class: 'meta' }, beekeeperAuthorLabel(m.author), ' · ', when(m.at)),
        el('div', { class: 'text', 'data-field': 'text' }, m.text)))));
    }
    if (ui.view === 'beekeeper') {
      byId('main').scrollTop = byId('main').scrollHeight;
    }
  }

  // openBeekeeper shows the Beekeeper chat and reads its recent messages
  // again. It is service-level, not tied to any selected workstream.
  function openBeekeeper() {
    openView('beekeeper');
    mark(['beekeeper']);
  }

  // sendBeekeeper posts an owner message to the Beekeeper and, once it
  // answers, reads the chat again, the same way the chief-of-staff
  // composer's send marks the workstream's feed dirty after posting: the
  // sent message and the reply appear without a page reload. A busy
  // refusal from the API shows in the composer's result.
  function sendBeekeeper(event) {
    event.preventDefault();
    const form = byId('beekeeper-form');
    const result = byId('beekeeper-result');
    const text = form.elements.text.value.trim();
    if (text === '') {
      show(result, 'error', 'Write a message first.');
      return;
    }
    act(result, event.submitter || form.querySelector('button'), () => request('POST', '/beekeeper', { text }), () => {
      form.elements.text.value = '';
      mark(['beekeeper']);
      return 'Sent; the beekeeper has replied.';
    });
  }

  function renderBeekeeperButton() {
    byId('beekeeper-button').setAttribute('aria-current', String(ui.view === 'beekeeper'));
  }

  function setTab(tab) {
    const changed = ui.tab !== tab;
    ui.tab = tab;
    render();
    if (changed && tab === 'documents') {
      readWorkstreamDocuments();
    }
    if (changed && tab === 'feed') {
      ui.toEnd = true;
      render();
    }
  }

  // inboxResult shows the outcome of the last answer to an inbox entry. It
  // moves to the card of the workstream shown, so it is held here rather
  // than looked up in the document.
  const inboxResult = byId('inbox-result');

  // cards keeps each workstream's card between renders, so a message being
  // written survives the events that arrive meanwhile and a switch to
  // another workstream and back.
  const cards = new Map();

  function send(id, c) {
    const text = c.text.value.trim();
    if (text === '') {
      show(c.result, 'error', 'Write a message first.');
      return;
    }
    act(c.result, c.button, () => request('POST', '/conversation/' + id, { text }), (entry) => {
      c.text.value = '';
      ui.toEnd = true;
      mark(['feed/' + id]);
      return 'Sent; the chief of staff answers in its next turn (' + entry.state + ').';
    });
  }

  function card(id) {
    let c = cards.get(id);
    if (c) {
      return c;
    }
    c = {
      entries: el('div', { class: 'feed', 'data-field': 'feed' }),
      heading: el('h3', { class: 'needs-title' }, 'Waiting for you'),
      decisions: el('div', { class: 'decisions', 'data-field': 'decisions' }),
      text: el('textarea', { name: 'text', rows: '2', 'aria-label': 'Message to the chief of staff', placeholder: 'Message the chief of staff' }),
      button: el('button', { type: 'submit', class: 'primary' }, 'Send'),
      result: el('p', { class: 'result', role: 'status' }),
      shown: null,
      // opened holds the sessions whose report the owner expanded, so a
      // new entry does not fold them again.
      opened: new Set(),
    };
    const form = el('form', { class: 'send composer', 'data-field': 'send' },
      el('div', { class: 'box' }, c.text, el('div', { class: 'actions' }, c.button)), c.result);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      send(id, c);
    });
    // Enter sends and Shift+Enter starts a new line where there is a
    // keyboard; on a touch screen Enter starts a new line.
    c.text.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && window.matchMedia('(pointer: fine)').matches) {
        event.preventDefault();
        form.requestSubmit();
      }
    });
    c.node = el('article', { class: 'workstream', 'data-workstream': id },
      el('div', { class: 'thread' }, c.entries, c.decisions, form));
    cards.set(id, c);
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

  function renderConversationEntry(e) {
    const who = { message: 'You', action: 'Chief of staff acted' }[e.kind] || 'Chief of staff';
    return el('li', { class: 'entry', 'data-kind': e.kind, 'data-turn': e.turn, 'data-state': e.state },
      el('div', { class: 'meta' },
        el('span', { 'data-ann': 'role' }, who), ' · ',
        el('span', { 'data-ann': 'when' }, when(e.at)), ' · ',
        el('span', { 'data-field': 'state', 'data-ann': 'action' }, e.state)),
      el('div', { class: 'text', 'data-field': 'text' }, e.text));
  }

  // renderStatus shows a status the chief of staff wrote. Only the latest
  // status's attention asks for something now.
  function renderStatus(e, latest) {
    const s = e.status;
    return el('li', { class: 'entry', 'data-kind': 'status', 'data-revision': String(s.revision), 'data-latest': String(latest) },
      el('div', { class: 'meta' },
        el('span', { 'data-ann': 'role' }, 'Chief of staff'), ' · ',
        el('span', { 'data-ann': 'action' }, 'status'), ' · ',
        el('span', { 'data-ann': 'when' }, when(e.at))),
      el('div', { class: 'card' },
        s.attention ? el('p', latest ? { class: 'attention', 'data-field': 'attention' } : { class: 'meta', 'data-field': 'past-attention' }, s.attention) : null,
        el('p', { class: 'text', 'data-field': 'note' }, s.note),
        s.agents.length > 0 ? el('ul', { class: 'notes', 'data-field': 'status-agents' }, ...s.agents.map((a) => el('li', {}, a))) : null));
  }

  function renderSession(e, c) {
    const s = e.session;
    const report = s.failure || s.summary;
    const line = el('span', { 'data-field': 'session' },
      el('strong', { 'data-ann': 'role' }, role(s.role)), s.unit ? ' on ' + s.unit : '',
      ' · ', el('span', { 'data-field': 'profile' }, s.profile),
      ' · ', el('span', { 'data-field': 'outcome', 'data-ann': 'action' }, sessionOutcome(s)));
    const head = [el('span', { class: 'dot', 'aria-hidden': 'true' }), line, el('span', { class: 'meta', 'data-ann': 'when' }, when(s.started_at))];
    const node = el('li', { class: 'entry event', 'data-kind': 'session', 'data-role': s.role, 'data-state': s.state, 'data-turn': s.turn });
    if (!report) {
      node.append(el('div', { class: 'event-line' }, ...head));
      return node;
    }
    const details = el('details', {}, el('summary', { class: 'event-line' }, ...head),
      el('div', { class: 'text', 'data-field': s.failure ? 'failure' : 'summary' }, report));
    details.open = c.opened.has(s.agent + '/' + s.turn);
    details.addEventListener('toggle', () => {
      if (details.open) {
        c.opened.add(s.agent + '/' + s.turn);
      } else {
        c.opened.delete(s.agent + '/' + s.turn);
      }
    });
    node.append(details);
    return node;
  }

  function renderTransition(e) {
    const t = e.transition;
    return el('li', { class: 'entry event', 'data-kind': 'transition', 'data-unit': t.unit || '', 'data-to': t.to },
      el('div', { class: 'event-line', title: t.reason },
        el('span', { class: 'dot', 'aria-hidden': 'true' }),
        el('span', { 'data-field': 'transition' },
          el('span', { 'data-ann': 'role' }, t.unit ? 'Unit ' + t.unit : 'Workstream'), ': ', t.from ? t.from + ' → ' : '',
          el('strong', { 'data-ann': 'action' }, t.to)),
        t.reason ? el('span', { class: 'reason' }, t.reason) : null,
        el('span', { class: 'meta', 'data-ann': 'when' }, when(e.at))));
  }

  // renderFeed shows the workstream's feed: the conversation with the chief
  // of staff, its statuses, the sessions that ran and the state changes, in
  // the order they happened.
  function renderFeed(c, id) {
    const name = 'feed/' + id;
    const view = views[name];
    const key = view ? JSON.stringify(view.entries) : (failures[name] ? 'failed' : 'reading');
    if (c.shown === key) {
      return;
    }
    c.shown = key;
    if (!view) {
      c.entries.replaceChildren(el('p', { class: 'meta' }, failures[name] ? 'The feed is unavailable.' : 'Reading the feed…'));
      return;
    }
    if (view.entries.length === 0) {
      c.entries.replaceChildren(el('p', { class: 'meta' }, 'Nothing has happened yet.'));
      return;
    }
    const latest = view.entries.findLastIndex((e) => e.kind === 'status');
    c.entries.replaceChildren(el('ol', {}, ...view.entries.map((e, i) => {
      switch (e.kind) {
        case 'status':
          return renderStatus(e, i === latest);
        case 'session':
          return renderSession(e, c);
        case 'transition':
          return renderTransition(e);
        default:
          return renderConversationEntry(e);
      }
    })));
  }

  // renderDecisions shows what waits for the owner on the workstream above
  // its message field, with the outcome of the last answer.
  function renderDecisions(c, id) {
    const nodes = [...inboxOf(id).map(renderDecision), ...proposalsOf(id).map(renderProposal)];
    place(c.decisions, [...(nodes.length > 0 ? [c.heading] : []), ...nodes, inboxResult]);
  }

  // renderUnitCounts counts the workstream's units by state, naming them on
  // hover.
  function renderUnitCounts(units) {
    if (!units || units.length === 0) {
      return null;
    }
    const byState = new Map(unitOrder.map((state) => [state, []]));
    for (const u of units) {
      if (!byState.has(u.state)) {
        byState.set(u.state, []);
      }
      byState.get(u.state).push(u.unit);
    }
    return el('div', { class: 'unit-counts', 'data-field': 'units' }, ...[...byState]
      .filter(([, names]) => names.length > 0)
      .map(([state, names]) => el('span', { class: 'tag', 'data-unit-state': state, title: names.join(', ') }, state + ' ' + names.length)));
  }

  function renderHead(w) {
    const head = byId('workstream-head');
    const status = w.status;
    head.dataset.workstream = w.workstream;
    // The menu offers what the workstream's state allows: asking for a drift
    // rebase of a building or assembled branch, recording your own merge of an
    // assembled branch, abandoning work in progress, archiving finished work
    // with permanent trace cleanup.
    const offered = {
      rebase: w.state === 'building' || w.state === 'assembled',
      merged: w.state === 'assembled',
      abandon: !terminal(w),
      'abandon-archive': !terminal(w),
      archive: terminal(w) && !w.archived,
    };
    for (const button of document.querySelectorAll('[data-workstream-action]')) {
      const action = button.dataset.workstreamAction;
      button.hidden = action in offered && !offered[action];
    }
    head.replaceChildren(...[
      el('div', { class: 'line' },
        el('h2', { 'data-field': 'goal' }, status ? status.goal : 'No status yet'),
        w.state ? el('span', { class: 'tag', 'data-field': 'state' }, w.state) : '',
        w.archived ? el('span', { class: 'tag', 'data-field': 'archived' }, 'archived') : ''),
      el('div', { class: 'id' }, el('span', { 'data-field': 'project' }, projectName(w.project)), ' · ', w.workstream, ' · ', el('span', { 'data-field': 'workspaces' }, w.workspaces + ' workspaces')),
      renderUnitCounts(w.units)].filter((node) => node !== null));
  }

  function renderWorkstream(w) {
    const c = card(w.workstream);
    renderFeed(c, w.workstream);
    renderDecisions(c, w.workstream);
    return c.node;
  }

  function renderWorkstreamView() {
    const box = byId('workstreams');
    const ids = new Set(streams().map((w) => w.workstream));
    for (const id of cards.keys()) {
      if (!ids.has(id)) {
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
    const main = byId('main');
    const atEnd = main.scrollHeight - main.scrollTop - main.clientHeight < 48;
    renderHead(w);
    place(box, [renderWorkstream(w)]);
    if (ui.view === 'workstream' && ui.tab === 'feed' && (ui.toEnd || atEnd)) {
      main.scrollTop = main.scrollHeight;
      ui.toEnd = false;
    }
  }

  // renderHeader shows the slots in use per role, the pauses in force and
  // whether a reload has something to apply.
  function renderHeader() {
    const capacity = views.status ? views.status.capacity : null;
    const meters = byId('meters');
    if (!capacity) {
      meters.replaceChildren();
      byId('meter-total').textContent = '';
    } else {
      const used = capacity.roles.reduce((n, r) => n + r.used, 0);
      const limit = capacity.roles.reduce((n, r) => n + r.limit, 0);
      meters.replaceChildren(...capacity.roles.map((r) => {
        const fill = el('span', { class: 'fill' });
        fill.style.height = (r.limit > 0 ? Math.min(100, Math.round(100 * r.used / r.limit)) : 0) + '%';
        return el('span', { class: 'meter', 'data-meter': r.role, 'data-full': String(r.used >= r.limit), 'data-waiting': String(r.waiting.length > 0),
          title: r.role + ': ' + r.used + ' of ' + r.limit + ' slots' + (r.waiting.length > 0 ? ', ' + r.waiting.length + ' waiting' : '') }, fill);
      }));
      byId('meter-total').textContent = used + '/' + limit;
      byId('capacity-button').title = used + ' of ' + limit + ' slots in use';
    }
    const pauses = views.runtime ? views.runtime.effective.pauses || [] : [];
    const button = byId('pause-button');
    button.dataset.paused = String(pauses.length > 0);
    button.title = pauses.length === 0 ? 'Nothing is paused' : pauses.length + ' paused';
    byId('pause-count').textContent = pauses.length > 0 ? String(pauses.length) : '';
    const lit = views.config ? reloadable(views.config) : false;
    byId('settings-button').dataset.lit = String(lit);
    byId('config-lit').hidden = !lit;
  }

  function renderViews() {
    for (const view of document.querySelectorAll('#main > .view')) {
      view.hidden = view.dataset.view !== ui.view;
    }
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
      if (!window.confirm('Permanently archive ' + goal(w) + '? All workstream history, decisions, agent turns and check output will be deleted. Only its title will remain in the archived list. This cannot be undone.')) { return; }
      const button = document.querySelector('[data-workstream-action="' + action + '"]');
      act(inboxResult, button, () => request('POST', '/archive/' + w.workstream), (out) => {
        if (out.archived) {
          leaveArchived(w.workstream);
        }
        mark(['status', 'runtime']);
        return 'Archived ' + goal(w) + '; it is listed under Archived. Its history will be permanently deleted.';
      });
      return;
    }
    if (action === 'rebase') {
      const button = document.querySelector('[data-workstream-action="rebase"]');
      act(inboxResult, button, () => request('POST', '/rebase/' + w.workstream), (out) => {
        mark(['status', 'inbox', 'feed/' + w.workstream]);
        return 'Drift rebase ' + out.drift + ' of ' + goal(w) + ' is requested; the foreman runs it once no landing or other drift rebase holds the project.';
      });
      return;
    }
    if (action === 'merged') {
      const button = document.querySelector('[data-workstream-action="merged"]');
      act(inboxResult, button, () => request('POST', '/delivery/' + w.workstream + '/merged'), (out) => {
        mark(['status', 'runtime', 'inbox', 'feed/' + w.workstream]);
        return goal(w) + ' is delivered: ' + out.merge.upstream.remote + '/' + out.merge.upstream.branch + ' holds ' + out.merge.branch + '.';
      });
      return;
    }
    setTab('documents');
    const target = byId(action === 'dependency' ? 'base-form' : 'workstream-action');
    if (action === 'abandon' || action === 'abandon-archive') {
      target.elements.action.value = 'abandon';
      target.elements.archive.checked = action === 'abandon-archive';
      renderOwnerForms();
      target.elements.note.focus();
    } else if (action === 'debate') {
      target.elements.action.focus();
    }
    target.scrollIntoView({ block: 'start' });
  }

  function setupLayout() {
    byId('picker-toggle').addEventListener('click', () => {
      setPicker(document.body.dataset.picker !== 'open');
      render();
    });
    byId('picker-close').addEventListener('click', () => {
      setPicker(false);
      render();
    });
    for (const id of ['new-workstream', 'sidebar-new']) {
      byId(id).addEventListener('click', () => openView('handin'));
    }
    byId('beekeeper-button').addEventListener('click', openBeekeeper);
    for (const button of document.querySelectorAll('[data-open]')) {
      button.addEventListener('click', () => openView(button.dataset.open));
    }
    for (const button of document.querySelectorAll('[data-close]')) {
      button.addEventListener('click', () => openView('workstream'));
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
    delivery: 'Delivery',
    publication: 'Publication failing',
    drift: 'Drift held',
    notices: 'Notices held',
    loop: 'Loop guard',
    base: 'Base parked',
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
  };

  const decisionNames = {
    review: 'Review the unit again',
    revise: 'Revise the unit',
    approve: 'Approve',
    reject: 'Reject',
    round: 'Debate another round',
    overrule: 'Overrule the objections',
    upstream: 'Continue from upstream',
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
    return out.join(' · ');
  }

  function decisionKey(entry) {
    return [entry.kind, entry.project, entry.workstream, entry.number, entry.unit, entry.amendment].join(':');
  }

  // decisions keeps each inbox entry's card between renders, so an answer
  // being written and a decision being chosen survive the events that
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
    act(inboxResult, button, () => request(method, path, body), (out) => {
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

  function accept(d) {
    const entry = d.entry;
    submit(d.form, d.accept, entry.answer.method, endpoint(entry), { ...entry.answer.body, text: entry.quick_reply },
      (out) => 'Accepted the recommendation on inbox entry ' + out.number + '.', undefined, decisionKey(entry));
  }

  function decide(d) {
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
    if (entry.kind === 'delivery') {
      body.messages = d.messages.map((m) => ({ commit: m.commit, message: m.input.value }));
      body.description = d.text.value;
      submit(d.form, d.submit, entry.answer.method, endpoint(entry), body,
        (out) => 'Approved the delivery of final review ' + out.review + ' of ' + workstreamName(entry.workstream) + '.',
        // The next render then treats the drafted description and commit
        // messages as unedited, so it fills them with whatever the delivery
        // presents next rather than leaving the reset, blank fields stuck.
        () => { d.draft = ''; d.messages = []; }, decisionKey(entry));
      return;
    }
    const [decision, objection] = d.decision.value.split(' ');
    const note = d.note.value.trim();
    if (!decision) {
      show(result, 'error', 'Choose a decision.');
      return;
    }
    switch (entry.kind) {
      case 'contested':
        if (note === '') {
          show(result, 'error', 'Give a note for the ruling.');
          return;
        }
        submit(d.form, d.submit, entry.answer.method, endpoint(entry), { ...body, decision, note },
          () => 'Ruled ' + decision + ' on unit ' + entry.unit + '.', undefined, decisionKey(entry));
        return;
      case 'amendment':
        if (note !== '') {
          body.note = note;
        }
        submit(d.form, d.submit, entry.answer.method, endpoint(entry), { ...body, decision },
          () => 'Decided ' + decision + ' on amendment ' + entry.amendment + '.', undefined, decisionKey(entry));
        return;
      case 'base':
        submit(d.submit, entry.answer.method, endpoint(entry), body, decided((out) => workstreamName(entry.workstream) + ' continues from ' + out.upstream.remote + '/' + out.upstream.branch + '.'));
        return;
    }
    // A ratification is decided with ratify, which dismisses its card like
    // any other terminal decision, or through the shed: a disposition of
    // an objection or a request for a redraft, which the shed may still
    // leave open with updated dissent, so those do not dismiss it.
    if (decision === 'ratify') {
      submit(d.form, d.submit, entry.answer.method, endpoint(entry), body, (out) => out.detail, undefined, decisionKey(entry));
      return;
    }
    if (decision === 'sustain') {
      submit(d.form, d.submit, 'POST', '/shed/rule/' + entry.workstream, { objection, disposition: 'sustain', note }, (out) => out.detail);
      return;
    }
    if (decision === 'overrule') {
      submit(d.form, d.submit, 'POST', '/shed/overrule/' + entry.workstream, { objection, reason: note }, (out) => out.detail);
      return;
    }
    if (decision === 'redraft') {
      if (note === '') {
        show(result, 'error', 'Say what the redraft should change.');
        return;
      }
      submit(d.form, d.submit, 'POST', '/shed/redraft/' + entry.workstream, { note }, (out) => out.detail);
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
      submit: el('button', { type: 'submit' }, { escalation: 'Answer', delivery: 'Approve' }[entry.kind] || 'Decide'),
      shown: null,
    };
    const fields = [];
    if (entry.kind === 'escalation') {
      d.text = el('textarea', { name: 'text', rows: '2', 'aria-label': 'Your answer' });
      d.accept = el('button', { type: 'button', 'data-field': 'accept' });
      d.accept.addEventListener('click', () => accept(d));
      fields.push(d.text);
    } else if (entry.kind === 'delivery') {
      d.text = el('textarea', { name: 'description', rows: '8' });
      d.draft = '';
      d.messages = [];
      d.messageFields = el('div');
      fields.push(d.messageFields);
      fields.push(el('label', {}, 'Pull request description', d.text));
      fields.push(el('p', { class: 'meta' }, 'Approving authorizes publication with your Git identity, signature and sign-off. Agent co-author trailers are removed.'));
    } else {
      d.decision = el('select', { name: 'decision' });
      d.note = el('input', { name: 'note', type: 'text', autocomplete: 'off' });
      fields.push(el('label', {}, 'Decision', d.decision), el('label', {}, 'Note', d.note));
    }
    const form = el('form', { class: 'send', 'data-field': 'answer' }, ...fields, d.actions);
    d.form = form;
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      decide(d);
    });
    // A failing publication, a held drift rebase, held notices or a loop
    // guard pause take no decision here, so their cards have no form.
    d.node = el('article', { class: 'decision', 'data-decision': key, 'data-kind': entry.kind, 'data-workstream': entry.workstream }, d.head, d.detail, ['publication', 'drift', 'notices', 'loop'].includes(entry.kind) ? null : form);
    decisions.set(key, d);
    return d;
  }

  function renderDissent(d, name) {
    const view = views[name];
    if (!view) {
      d.detail.replaceChildren(el('p', { class: 'meta' }, failures[name] ? 'The packet is unavailable.' : 'Reading the packet…'));
      return [];
    }
    const dissent = view.packet.dissent || [];
    d.detail.replaceChildren(dissent.length === 0 ? el('p', { class: 'meta' }, 'No objection stands.') : el('ul', { class: 'dissent' }, ...dissent.map((o) => el('li', { 'data-objection': o.id },
      el('div', { class: 'line' },
        el('strong', {}, o.id),
        el('span', { class: 'tag', 'data-field': 'standing' }, [o.blocking ? 'blocking' : 'advice', o.disposition].filter(Boolean).join(', '))),
      el('div', { class: 'meta' }, o.kind + ' by ' + o.member + ' in round ' + o.round + ' on ' + o.part),
      el('div', { 'data-field': 'argument' }, o.argument),
      o.note ? el('div', { class: 'meta', 'data-field': 'note' }, 'Your note: ' + o.note) : null))));
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
  // Approve waits until they are shown, so an approval never pins a draft
  // the page does not show.
  function renderDelivery(d, name) {
    const view = presented(d.entry, name);
    block(d.submit, !view);
    if (!view) {
      d.detail.replaceChildren(el('p', { class: 'meta', 'data-field': 'reading' }, failures[name] ? 'The final report is unavailable.' : 'Reading the final report…'));
      return;
    }
    d.detail.replaceChildren(el('ul', { class: 'criteria', 'data-field': 'criteria' }, ...view.report.criteria.map((c) => el('li', { 'data-criterion': c.criterion },
      el('strong', {}, c.criterion), ' ', c.text,
      el('div', { class: 'meta' }, c.evidence ? c.evidence : 'Gap: ' + c.gap)))));
    const previous = new Map(d.messages.map((m) => [m.commit, m]));
    d.messages = (view.approval ? view.approval.messages : (view.messages || [])).map((m) => {
      const old = previous.get(m.commit);
      const input = old ? old.input : el('textarea', { name: 'commit-message', rows: '4' });
      if (!old || input.value === old.draft) input.value = m.message;
      return { commit: m.commit, draft: m.message, input };
    });
    d.messageFields.replaceChildren(...d.messages.map((m, i) => el('label', {}, 'Commit message' + (d.messages.length > 1 ? ' ' + (i + 1) : ''), m.input)));
    // The draft replaces the description until the owner edits it.
    const draft = view.approval ? view.approval.description : view.draft;
    if (draft !== d.draft) {
      if (d.text.value === d.draft) {
        d.text.value = draft;
      }
      d.draft = draft;
    }
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
    let options = null;
    if (entry.kind === 'escalation') {
      options = entry.options.length === 0 ? null : el('div', { class: 'actions', 'data-field': 'options' }, 'Options:', ...entry.options.map((o) => {
        const button = el('button', { type: 'button', 'data-field': 'option' }, o);
        button.addEventListener('click', () => {
          d.text.value = o;
        });
        return button;
      }));
    } else {
      const none = ['ratification', 'publication', 'drift', 'notices', 'loop', 'base'].includes(entry.kind) ? 'none until what blocks it is resolved' : 'none';
      options = el('p', { 'data-field': 'options' }, 'Options: ' + (entry.options.length === 0 ? none : entry.options.join(', ')));
    }
    d.head.replaceChildren(...[
      el('div', { class: 'line' },
        el('span', { class: 'tag', 'data-field': 'kind' }, kinds[entry.kind] || entry.kind),
        el('span', { class: 'meta' }, when(entry.opened_at))),
      el('p', { class: 'question', 'data-field': 'question' }, entry.question),
      entry.asked.length === 0 ? null : el('ul', { class: 'asked', 'data-field': 'asked' }, ...entry.asked.map((q) => el('li', {},
        el('span', { class: 'meta' }, q.asked_by + (q.unit ? ' on ' + q.unit : '') + ': '), q.question))),
      entry.blocked ? el('p', { class: 'meta', 'data-field': 'blocked' }, 'Waiting on it: ' + entry.blocked) : null,
      options,
      entry.recommendation ? el('p', { class: 'attention', 'data-field': 'recommendation' }, 'Recommendation: ' + entry.recommendation) : null,
      entry.kind === 'publication' ? null : el('p', { class: 'meta', 'data-field': 'pins' }, 'Answers ' + pins(entry)),
    ].filter((node) => node !== null));
    switch (entry.kind) {
      case 'escalation':
        d.accept.textContent = 'Accept: ' + entry.quick_reply;
        place(d.actions, entry.quick_reply ? [d.submit, d.accept] : [d.submit]);
        break;
      case 'delivery':
        renderDelivery(d, name);
        place(d.actions, [d.submit]);
        break;
      case 'publication':
        break;
      case 'ratification': {
        const dissent = renderDissent(d, name);
        setOptions(d.decision, [['', 'Choose a decision'],
          ...(entry.options.includes('ratify') ? [['ratify', 'Ratify']] : []),
          ...dissent.flatMap((o) => [['sustain ' + o.id, 'Sustain ' + o.id], ['overrule ' + o.id, 'Overrule ' + o.id]]),
          ['redraft', 'Ask for a redraft']]);
        place(d.actions, [d.submit]);
        break;
      }
      default:
        setOptions(d.decision, [['', 'Choose a decision'], ...entry.options.map((o) => [o, decisionNames[o] || o])]);
        // A parked base takes no note, and no decision here unless its own
        // base was abandoned.
        if (entry.kind === 'base') {
          d.form.hidden = entry.options.length === 0;
          d.note.parentElement.hidden = true;
        }
        place(d.actions, [d.submit]);
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
          act(inboxResult, button, () => request('POST', '/charter/' + p.workstream + '/' + p.question, { decision }),
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
    return projects().some((p) => p.id === id) ? id : null;
  }

  function renderPriority() {
    const current = byId('priority-current');
    const list = byId('priority-list');
    if (!views.status || !views.runtime || !views.config) {
      return;
    }
    setOptions(byId('priority-project'), projects().map((p) => [p.id, projectName(p.id)]));
    const id = priorityProject();
    block(byId('priority-form').querySelector('button[type=submit]'), !id);
    block(byId('priority-clear'), !id);
    if (!id) {
      current.textContent = 'No project is configured.';
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
      show(result, 'error', 'No project is configured.');
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
      show(result, 'error', 'No project is configured.');
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
      el('div', { class: 'line' }, el('strong', {}, role), el('span', {}, r.profile, ' ', r.source)),
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
    const options = Object.keys(configured).sort().map((name) => [name, name + ' · ' + configured[name].agent]);
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
      r.profile.textContent = effective.name ? effective.name + (provider ? ' · ' + provider : '') : 'none';
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
  let draftRead = null;
  let baseRead = null;
  let handinRetry = null;

  // formAction runs an owner action a form submits. With opts.reset, a
  // successful submission clears the form back to its default values, the
  // way the browser's own reset does, and recomputes what renderOwnerForms
  // keeps current, such as a hidden workstream field; a failed submission
  // leaves the form exactly as the owner left it.
  function formAction(form, button, operation, done, opts) {
    return act(form.querySelector('.result'), button, operation, (out) => {
      mark(['status', 'config', 'inbox', 'charter']);
      const text = done(out);
      if (opts && opts.reset) {
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
    for (const id of workstreamForms) {
      show(byId(id).querySelector('.result'), '', '');
    }
    show(inboxResult, '', '');
  }

  function renderOwnerForms() {
    const options = [['', 'Choose a project'], ...projects().map(p => [p.id, projectName(p.id)])];
    for (const select of document.querySelectorAll('[data-project-select]')) {
      setOptions(select, options);
    }
    const all = streams();
    const handin = byId('handin-form');
    setOptions(handin.elements.base, [['', 'Project upstream'], ...all.filter(w => w.project === handin.elements.project.value && w.state !== 'abandoned' && !w.archived).map(w => [w.workstream, goal(w)])]);
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
      formAction(add, event.submitter, () => request('POST', '/projects', body), out => 'Registered ' + out.project.name + '. Read and write its charter in Projects and charters before handing in work.', { reset: true });
    });
    const handin = byId('handin-form');
    handin.elements.project.addEventListener('change', renderOwnerForms);
    handin.addEventListener('submit', event => {
      event.preventDefault();
      const body = {project: handin.elements.project.value, base: handin.elements.base.value, skip_debate: handin.elements.skip_debate.checked};
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
      charterRead = null;
      charter.elements.content.value = '';
      renderOwnerForms();
    });
    charter.querySelector('[data-action=read-charter]').addEventListener('click', event => {
      const project = charter.elements.project.value;
      if (!project) { show(charter.querySelector('.result'), 'error', 'Choose a project.'); return; }
      formAction(charter, event.target, () => request('GET', '/projects/charter/' + project), out => {
        if (charter.elements.project.value === project) {
          charterRead = {project, revision: out.revision};
          charter.elements.content.value = out.content;
        }
        renderOwnerForms();
        return 'Read charter revision ' + out.revision + '.';
      });
    });
    charter.addEventListener('submit', event => {
      event.preventDefault();
      if (!charterRead || charterRead.project !== charter.elements.project.value) { return; }
      const pin = charterRead;
      formAction(charter, event.submitter, () => request('PUT', '/projects/charter/' + pin.project,
        {revision: pin.revision, content: charter.elements.content.value}), out => {
          if (charterRead === pin) { charterRead = {project: pin.project, revision: out.revision}; }
          return 'Saved charter revision ' + out.revision + '.';
        });
    });
    for (const action of ['extract', 'rebase', 'remove']) {
      charter.querySelector('[data-action=' + action + ']').addEventListener('click', event => {
        const project = charter.elements.project.value;
        if (!project) { show(charter.querySelector('.result'), 'error', 'Choose a project.'); return; }
        formAction(charter, event.target, () => request(action === 'remove' ? 'DELETE' : 'POST',
          action === 'remove' ? '/projects' : '/projects/' + action, {project}),
          () => action === 'remove' ? 'Project removed; its trace and clone are retained.' : 'Project ' + action + ' requested.');
      });
    }
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
      const bodies = {object: {argument: note}, more: {rounds: 1}, redraft: {note}, skip: {}, abandon: {reason: note}};
      const archive = kind === 'abandon' && action.elements.archive.checked;
      formAction(action, event.submitter, async () => {
        const out = await request('POST', (kind === 'abandon' ? '/abandon/' : '/shed/' + kind + '/') + workstream, bodies[kind]);
        if (archive) {
          await request('POST', '/archive/' + workstream);
        }
        return out;
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
      const path = '/trace/' + trace.elements.workstream.value + (kind ? '/' + kind + '/' + encodeURIComponent(selector) : '');
      formAction(trace, event.submitter, () => request('GET', path), out => {
        byId('trace-content').replaceChildren(traceTree(out));
        return 'Trace loaded.';
      });
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

  function render() {
    forget();
    pruneInbox();
    markSeen();
    renderViews();
    renderOwnerForms();
    renderProblems();
    renderHeader();
    renderSidebar();
    renderWorkstreamView();
    renderPauses();
    renderPauseForm();
    renderCapacity();
    renderPriority();
    renderProfiles();
    renderProviders();
    renderConfig();
    renderBeekeeperButton();
    renderBeekeeperFeed();
  }

  setupLayout();
  setupOwnerForms();
  render();
  byId('beekeeper-form').addEventListener('submit', sendBeekeeper);
  // Enter sends and Shift+Enter starts a new line where there is a
  // keyboard; on a touch screen Enter starts a new line.
  byId('beekeeper-form').elements.text.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && window.matchMedia('(pointer: fine)').matches) {
      event.preventDefault();
      byId('beekeeper-form').requestSubmit();
    }
  });
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
