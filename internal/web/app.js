'use strict';

(() => {
  const api = '/v1';

  // The views each event kind names. "conversation" is the conversation of
  // the event's workstream and "conversations" that of every workstream the
  // page shows. Kinds the page does not show are ignored; every stream starts
  // with a resync.
  const reads = {
    resync: ['status', 'runtime', 'config', 'inbox', 'conversations'],
    workstream: ['status'],
    conversation: ['conversation'],
    inbox: ['inbox'],
    runtime: ['runtime', 'status'],
    config: ['config'],
    spend: ['status'],
  };

  // An edit of a configuration file on disk is not announced, so the page
  // reads /config again this often while it is visible and live.
  const configInterval = 10000;

  const unitOrder = ['planned', 'ready', 'implementing', 'waiting', 'reviewing', 'approved', 'contested', 'merged'];

  const pauseSources = {
    owner: 'the owner',
    'daily-budget': 'the daily budget',
    'provider-usage-limit': 'a provider usage limit',
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
  // inbox, conversation/<workstream-id>, and packet/<workstream-id> and
  // delivery/<workstream-id> for the ratifications and deliveries the inbox
  // lists.
  const views = { status: null, runtime: null, config: null, inbox: null };
  const failures = {};
  const dirty = new Set();
  let reading = false;
  let retries = 0;
  let retryTimer = null;

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

  function listedConversations() {
    return views.status ? views.status.workstreams.map((w) => 'conversation/' + w.workstream) : [];
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
  // workstream the status lists gets its conversation read, and one it no
  // longer lists loses it. A ratification or delivery the inbox lists gets
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
          const listed = new Set(listedConversations());
          for (const name of [...Object.keys(views), ...Object.keys(failures)]) {
            if (name.startsWith('conversation/') && !listed.has(name)) {
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
    if (name === 'conversations') {
      return listedConversations();
    }
    if (name === 'conversation') {
      let data = {};
      try {
        data = JSON.parse(event.data);
      } catch {
        return [];
      }
      return data.workstream ? ['conversation/' + data.workstream] : [];
    }
    return [name];
  }

  function setConnection(state) {
    document.body.dataset.connection = state;
    const label = { connecting: 'Connecting…', live: 'Live', lost: 'Reconnecting…' }[state];
    byId('connection').textContent = label;
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

  function project() {
    const config = views.config;
    return config && config.project ? config.project.id : null;
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
  // project and each of its workstreams.
  function pauseTargets() {
    const targets = [['factory', 'The factory']];
    const id = project();
    if (id) {
      targets.push(['project:' + id, 'Project ' + id]);
      for (const w of views.status ? views.status.workstreams : []) {
        targets.push(['workstream:' + w.workstream, 'Workstream ' + goal(w)]);
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
    const [kind, id] = form.elements.target.value.split(':');
    const target = { scope: kind };
    if (kind === 'project' || kind === 'workstream') {
      target.project = project();
    }
    if (kind === 'workstream') {
      target.workstream = id;
    }
    const mode = form.elements.mode.value;
    const reason = form.elements.reason.value.trim();
    if (!kind || !pauseTargets().some(([value]) => value === form.elements.target.value)) {
      show(result, 'error', 'Choose what to pause.');
      return;
    }
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

  function renderUnits(units) {
    if (!units || units.length === 0) {
      return el('p', { class: 'meta' }, 'No units yet.');
    }
    const byState = new Map();
    for (const state of unitOrder) {
      byState.set(state, []);
    }
    for (const u of units) {
      if (!byState.has(u.state)) {
        byState.set(u.state, []);
      }
      byState.get(u.state).push(u.unit);
    }
    const rows = [];
    for (const [state, names] of byState) {
      if (names.length > 0) {
        rows.push(el('li', { 'data-state': state },
          el('span', { class: 'tag' }, state + ' ' + names.length),
          ' ', names.join(', ')));
      }
    }
    return el('ul', { class: 'units', 'data-field': 'units' }, ...rows);
  }

  function renderAgents(agents) {
    if (agents === null) {
      return el('p', { class: 'meta' }, 'Sessions are unavailable.');
    }
    if (agents.length === 0) {
      return el('p', { class: 'meta' }, 'No sessions running.');
    }
    return el('ul', { class: 'agents', 'data-field': 'agents' }, ...agents.map((a) => el('li', { 'data-role': a.role },
      el('strong', {}, a.role),
      a.unit ? ' on ' + a.unit : '',
      ' · ', el('span', { 'data-field': 'profile' }, a.profile),
      ' · ', a.state, ' ', duration(a.elapsed))));
  }

  // cards keeps each workstream's card between renders, so a message being
  // written survives the events that arrive meanwhile.
  const cards = new Map();

  function send(id, c) {
    const text = c.text.value.trim();
    if (text === '') {
      show(c.result, 'error', 'Write a message first.');
      return;
    }
    act(c.result, c.button, () => request('POST', '/conversation/' + id, { text }), (entry) => {
      c.text.value = '';
      mark(['conversation/' + id]);
      return 'Sent; the chief of staff answers in its next turn (' + entry.state + ').';
    });
  }

  function card(id) {
    let c = cards.get(id);
    if (c) {
      return c;
    }
    c = {
      summary: el('div', { class: 'summary' }),
      entries: el('div', { class: 'conversation', 'data-field': 'conversation' }),
      text: el('textarea', { name: 'text', rows: '2', 'aria-label': 'Message to the chief of staff' }),
      button: el('button', { type: 'submit' }, 'Send'),
      result: el('p', { class: 'result', role: 'status' }),
      shown: null,
    };
    const form = el('form', { class: 'send', 'data-field': 'send' }, c.text, el('div', { class: 'actions' }, c.button), c.result);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      send(id, c);
    });
    c.node = el('article', { class: 'workstream', 'data-workstream': id }, c.summary, el('h4', {}, 'Conversation'), c.entries, form);
    cards.set(id, c);
    return c;
  }

  function renderConversation(c, id) {
    const name = 'conversation/' + id;
    const view = views[name];
    const key = view ? JSON.stringify(view.entries) : (failures[name] ? 'failed' : 'reading');
    if (c.shown === key) {
      return;
    }
    c.shown = key;
    if (!view) {
      c.entries.replaceChildren(el('p', { class: 'meta' }, failures[name] ? 'The conversation is unavailable.' : 'Reading the conversation…'));
      return;
    }
    if (view.entries.length === 0) {
      c.entries.replaceChildren(el('p', { class: 'meta' }, 'No messages yet.'));
      return;
    }
    c.entries.replaceChildren(el('ol', {}, ...view.entries.map((e) => el('li', { class: 'entry', 'data-kind': e.kind, 'data-turn': e.turn, 'data-state': e.state },
      el('div', { class: 'meta' }, e.kind === 'message' ? 'You' : 'Chief of staff', ' · ', when(e.at), ' · ', el('span', { 'data-field': 'state' }, e.state)),
      el('div', { class: 'text', 'data-field': 'text' }, e.text)))));
    c.entries.scrollTop = c.entries.scrollHeight;
  }

  function renderWorkstream(w) {
    const status = w.status;
    const c = card(w.workstream);
    // replaceChildren turns a null child into the text "null", so the absent
    // ones are dropped first.
    c.summary.replaceChildren(...[
      el('div', { class: 'line' },
        el('h3', { 'data-field': 'goal' }, status ? status.goal : 'No status yet'),
        w.state ? el('span', { class: 'tag', 'data-field': 'state' }, w.state) : null),
      el('div', { class: 'id' }, w.workstream, ' · ', el('span', { 'data-field': 'workspaces' }, w.workspaces + ' workspaces')),
      status && status.attention ? el('p', { class: 'attention', 'data-field': 'attention' }, status.attention) : null,
      status ? el('p', { 'data-field': 'note' }, status.note) : el('p', { class: 'meta' }, 'The chief of staff has not written a status.'),
      el('h4', {}, 'Units'),
      renderUnits(w.units),
      el('h4', {}, 'Sessions'),
      renderAgents(w.agents)].filter((node) => node !== null));
    renderConversation(c, w.workstream);
    return c.node;
  }

  function renderWorkstreams() {
    const box = byId('workstreams');
    if (!views.status) {
      place(box, []);
      return;
    }
    const list = views.status.workstreams;
    for (const id of cards.keys()) {
      if (!list.some((w) => w.workstream === id)) {
        cards.delete(id);
      }
    }
    if (list.length === 0) {
      box.replaceChildren(el('p', { class: 'empty' }, 'No workstreams.'));
      return;
    }
    place(box, list.map(renderWorkstream));
  }

  const kinds = {
    escalation: 'Question',
    ratification: 'Ratification',
    contested: 'Contested unit',
    amendment: 'Amendment',
    delivery: 'Delivery',
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
  };

  const decisionNames = {
    review: 'Review the unit again',
    revise: 'Revise the unit',
    approve: 'Approve',
    reject: 'Reject',
    round: 'Debate another round',
    overrule: 'Overrule the objections',
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
    return [entry.kind, entry.workstream, entry.number, entry.unit, entry.amendment].join(':');
  }

  // decisions keeps each inbox entry's card between renders, so an answer
  // being written and a decision being chosen survive the events that
  // arrive meanwhile. A card answers the entry as it last rendered it.
  const decisions = new Map();

  // submit sends an answer. A refusal reads the inbox again, so the entry
  // shows what the refusal was about; the page never sends it again itself.
  function submit(button, method, path, body, done) {
    act(byId('inbox-result'), button, () => request(method, path, body), done, () => mark(['inbox']));
  }

  function endpoint(entry) {
    return entry.answer.path.slice(api.length);
  }

  function accept(d) {
    const entry = d.entry;
    submit(d.accept, entry.answer.method, endpoint(entry), { ...entry.answer.body, text: entry.quick_reply },
      (out) => 'Accepted the recommendation on inbox entry ' + out.number + '.');
  }

  function decide(d) {
    const entry = d.entry;
    const result = byId('inbox-result');
    const body = { ...entry.answer.body };
    if (entry.kind === 'escalation') {
      const text = d.text.value.trim();
      if (text === '') {
        show(result, 'error', 'Write an answer first.');
        return;
      }
      submit(d.submit, entry.answer.method, endpoint(entry), { ...body, text }, (out) => {
        d.text.value = '';
        return 'Answered inbox entry ' + out.number + '.';
      });
      return;
    }
    if (entry.kind === 'delivery') {
      if (d.text.value !== d.draft) {
        body.description = d.text.value;
      }
      submit(d.submit, entry.answer.method, endpoint(entry), body, (out) => 'Approved the delivery of final review ' + out.review + ' of ' + workstreamName(entry.workstream) + '.');
      return;
    }
    const [decision, objection] = d.decision.value.split(' ');
    const note = d.note.value.trim();
    // decided clears the choice once the API recorded it, so a card that
    // stays listed is not answered twice by accident.
    const decided = (text) => (out) => {
      d.decision.value = '';
      d.note.value = '';
      return text(out);
    };
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
        submit(d.submit, entry.answer.method, endpoint(entry), { ...body, decision, note }, decided(() => 'Ruled ' + decision + ' on unit ' + entry.unit + '.'));
        return;
      case 'amendment':
        if (note !== '') {
          body.note = note;
        }
        submit(d.submit, entry.answer.method, endpoint(entry), { ...body, decision }, decided(() => 'Decided ' + decision + ' on amendment ' + entry.amendment + '.'));
        return;
    }
    // A ratification is decided with ratify, or through the shed: a
    // disposition of an objection or a request for a redraft.
    const detail = decided((out) => out.detail);
    switch (decision) {
      case 'ratify':
        submit(d.submit, entry.answer.method, endpoint(entry), body, detail);
        return;
      case 'sustain':
        submit(d.submit, 'POST', '/shed/rule/' + entry.workstream, { objection, disposition: 'sustain', note }, detail);
        return;
      case 'overrule':
        submit(d.submit, 'POST', '/shed/overrule/' + entry.workstream, { objection, reason: note }, detail);
        return;
      case 'redraft':
        if (note === '') {
          show(result, 'error', 'Say what the redraft should change.');
          return;
        }
        submit(d.submit, 'POST', '/shed/redraft/' + entry.workstream, { note }, detail);
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
      fields.push(el('label', {}, 'Pull request description', d.text));
    } else {
      d.decision = el('select', { name: 'decision' });
      d.note = el('input', { name: 'note', type: 'text', autocomplete: 'off' });
      fields.push(el('label', {}, 'Decision', d.decision), el('label', {}, 'Note', d.note));
    }
    const form = el('form', { class: 'send', 'data-field': 'answer' }, ...fields, d.actions);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      decide(d);
    });
    d.node = el('article', { class: 'decision', 'data-decision': key, 'data-kind': entry.kind, 'data-workstream': entry.workstream }, d.head, d.detail, form);
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
    // The draft replaces the description until the owner edits it.
    if (view.draft !== d.draft) {
      if (d.text.value === d.draft) {
        d.text.value = view.draft;
      }
      d.draft = view.draft;
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
      const none = entry.kind === 'ratification' ? 'none until what blocks it is resolved' : 'none';
      options = el('p', { 'data-field': 'options' }, 'Options: ' + (entry.options.length === 0 ? none : entry.options.join(', ')));
    }
    d.head.replaceChildren(...[
      el('div', { class: 'line' },
        el('span', { class: 'tag', 'data-field': 'kind' }, kinds[entry.kind] || entry.kind),
        el('span', { class: 'meta' }, when(entry.opened_at))),
      el('div', {}, el('strong', { 'data-field': 'workstream' }, workstreamName(entry.workstream)), ' ', el('span', { class: 'id' }, entry.workstream)),
      el('p', { class: 'question', 'data-field': 'question' }, entry.question),
      entry.asked.length === 0 ? null : el('ul', { class: 'asked', 'data-field': 'asked' }, ...entry.asked.map((q) => el('li', {},
        el('span', { class: 'meta' }, q.asked_by + (q.unit ? ' on ' + q.unit : '') + ': '), q.question))),
      entry.blocked ? el('p', { class: 'meta', 'data-field': 'blocked' }, 'Waiting on it: ' + entry.blocked) : null,
      options,
      entry.recommendation ? el('p', { class: 'attention', 'data-field': 'recommendation' }, 'Recommendation: ' + entry.recommendation) : null,
      el('p', { class: 'meta', 'data-field': 'pins' }, 'Answers ' + pins(entry)),
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
        place(d.actions, [d.submit]);
    }
    return d.node;
  }

  function renderInbox() {
    const box = byId('inbox');
    if (!views.inbox) {
      place(box, []);
      return;
    }
    const entries = views.inbox.entries;
    const keys = new Set(entries.map(decisionKey));
    for (const key of decisions.keys()) {
      if (!keys.has(key)) {
        decisions.delete(key);
      }
    }
    if (entries.length === 0) {
      box.replaceChildren(el('p', { class: 'empty' }, 'Nothing waits for you.'));
      return;
    }
    place(box, entries.map(renderDecision));
  }

  // priority is the order being edited: the workstreams in the order shown
  // and those chosen to go first. It starts again from the order in force
  // whenever that changes.
  const priority = { inForce: null, order: [], chosen: new Set(), shown: null };

  function renderPriority() {
    const current = byId('priority-current');
    const list = byId('priority-list');
    const id = project();
    if (!views.status || !views.runtime || !views.config) {
      return;
    }
    if (!id) {
      current.textContent = 'No project is configured.';
      list.replaceChildren();
      return;
    }
    const record = (views.runtime.effective.priorities || []).find((p) => p.project === id);
    const inForce = record ? record.workstreams : [];
    const key = JSON.stringify(inForce);
    if (priority.inForce !== key) {
      priority.inForce = key;
      priority.order = [...inForce];
      priority.chosen = new Set(inForce);
    }
    const workstreams = views.status.workstreams;
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
    const shown = JSON.stringify([priority.order, [...priority.chosen], [...goals]]);
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
    const id = project();
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
    const id = project();
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

  function renderProviders() {
    const list = byId('providers');
    const usage = views.status ? views.status.provider_usage : null;
    if (!usage) {
      list.replaceChildren(el('li', { class: 'meta' }, views.status ? 'Provider usage is unavailable.' : ''));
      return;
    }
    const rows = usage.providers.map((p) => el('li', { 'data-provider': p.provider },
      el('div', { class: 'line' }, el('strong', {}, p.provider), el('span', { 'data-field': 'spend' }, money(p))),
      p.limit ? el('div', { class: 'attention', 'data-field': 'limit' }, limitText(p.limit)) : null,
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

  function render() {
    renderProblems();
    renderInbox();
    renderPauses();
    renderPauseForm();
    renderCapacity();
    renderWorkstreams();
    renderPriority();
    renderProfiles();
    renderProviders();
    renderConfig();
  }

  byId('pause-form').addEventListener('submit', pause);
  byId('priority-form').addEventListener('submit', setPriority);
  byId('priority-clear').addEventListener('click', clearPriority);
  byId('reload').addEventListener('click', reload);
  setInterval(() => {
    if (document.visibilityState === 'visible' && document.body.dataset.connection === 'live') {
      mark(['config']);
    }
  }, configInterval);
  window.addEventListener('online', reconnectNow);
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      reconnectNow();
      mark(['config']);
    }
  });
  connect();
})();
