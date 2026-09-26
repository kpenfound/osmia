'use strict';

(() => {
  const api = '/v1';

  // The views each event kind names. Kinds the page does not show are
  // ignored; every stream starts with a resync.
  const reads = {
    resync: ['status', 'runtime'],
    workstream: ['status'],
    runtime: ['runtime', 'status'],
    spend: ['status'],
  };

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

  const views = { status: null, runtime: null };
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

  async function fetchView(name) {
    const response = await fetch(api + '/' + name, { cache: 'no-store', headers: { Accept: 'application/json' } });
    const body = await response.json();
    if (!response.ok) {
      throw new Error(body.error && body.error.message ? body.error.message : response.statusText);
    }
    return body;
  }

  // refresh reads every view marked since the last read, once each, and
  // renders; views marked while it reads are read again after.
  async function refresh() {
    if (reading) {
      return;
    }
    reading = true;
    try {
      while (dirty.size > 0) {
        const names = [...dirty];
        dirty.clear();
        await Promise.all(names.map(async (name) => {
          try {
            views[name] = await fetchView(name);
            delete failures[name];
          } catch (err) {
            failures[name] = 'Cannot read /' + name + ': ' + err.message;
          }
        }));
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

  function setConnection(state) {
    document.body.dataset.connection = state;
    const label = { connecting: 'Connecting…', live: 'Live', lost: 'Reconnecting…' }[state];
    document.getElementById('connection').textContent = label;
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
      stream.addEventListener(kind, () => mark(names));
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
    const list = document.getElementById('problem-list');
    list.replaceChildren(...items.map((text) => el('li', {}, text)));
    document.getElementById('problems').hidden = items.length === 0;
  }

  function renderPauses() {
    const list = document.getElementById('pause-list');
    if (!views.runtime) {
      list.replaceChildren();
      return;
    }
    const pauses = views.runtime.effective.pauses || [];
    if (pauses.length === 0) {
      list.replaceChildren(el('li', { class: 'empty' }, 'Nothing is paused.'));
      return;
    }
    list.replaceChildren(...pauses.map((p) => el('li', { class: 'pause', 'data-pause': p.target.scope + ':' + (p.target.workstream || p.target.project || p.target.role || '') },
      el('div', { class: 'line' },
        el('strong', { 'data-field': 'scope' }, scope(p.target)),
        el('span', { class: 'tag', 'data-field': 'mode' }, p.mode)),
      el('div', { 'data-field': 'reason' }, p.reason),
      el('div', { class: 'meta' }, 'Set by ', el('span', { 'data-field': 'source' }, pauseSources[p.source] || p.source), ' at ', when(p.set_at)))));
  }

  function renderCapacity() {
    const box = document.getElementById('capacity');
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

  function renderWorkstream(w) {
    const status = w.status;
    return el('article', { class: 'workstream', 'data-workstream': w.workstream },
      el('div', { class: 'line' },
        el('h3', { 'data-field': 'goal' }, status ? status.goal : 'No status yet'),
        w.state ? el('span', { class: 'tag', 'data-field': 'state' }, w.state) : null),
      el('div', { class: 'id' }, w.workstream),
      status && status.attention ? el('p', { class: 'attention', 'data-field': 'attention' }, status.attention) : null,
      status ? el('p', { 'data-field': 'note' }, status.note) : el('p', { class: 'meta' }, 'The chief of staff has not written a status.'),
      el('h4', {}, 'Units'),
      renderUnits(w.units),
      el('h4', {}, 'Sessions'),
      renderAgents(w.agents));
  }

  function renderWorkstreams() {
    const box = document.getElementById('workstreams');
    if (!views.status) {
      box.replaceChildren();
      return;
    }
    const list = views.status.workstreams;
    if (list.length === 0) {
      box.replaceChildren(el('p', { class: 'empty' }, 'No workstreams.'));
      return;
    }
    box.replaceChildren(...list.map(renderWorkstream));
  }

  function render() {
    renderProblems();
    renderPauses();
    renderCapacity();
    renderWorkstreams();
  }

  window.addEventListener('online', reconnectNow);
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      reconnectNow();
    }
  });
  connect();
})();
