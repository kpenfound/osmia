'use strict';

const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright-core');

const VERBS = {
  goto: { min: 0, max: 1 },
  click: { min: 1, max: 1 },
  fill: { min: 2, max: 2 },
  press: { min: 1, max: 2 },
  snapshot: { min: 0, max: 0 },
  screenshot: { min: 0, max: 1 },
  console: { min: 0, max: 0 },
  requests: { min: 0, max: 0 },
};
const MAX_STEPS = 100;
const MAX_LOG_LINES = 2000;
const MAX_IMAGE_BYTES = 4 << 20;
const MAX_DETAIL = 600;

const config = {
  base: process.env.OSMIA_BASE_URL,
  steps: process.env.OSMIA_STEPS || '',
  flow: process.env.OSMIA_FLOW || '',
  out: process.env.OSMIA_OUT || '/out',
  timeout: Number(process.env.OSMIA_STEP_TIMEOUT_MS) || 10000,
};

const claims = [];
const lines = [];
const consoleLog = [];
const requestLog = [];
let consoleErrors = 0;
let requestProblems = 0;
let hasSnapshot = false;

function say(text) {
  lines.push(text);
}

function clip(text, limit) {
  const flat = String(text).replace(/\s+/g, ' ').trim();
  return flat.length > limit ? flat.slice(0, limit - 1) + '…' : flat;
}

function decode(text) {
  try {
    return decodeURIComponent(text);
  } catch (error) {
    return text;
  }
}

function parseSteps(text) {
  const steps = [];
  const parts = text.split(/[;\n]/).map((part) => part.trim()).filter(Boolean);
  if (parts.length === 0) {
    throw new Error('the step list is empty');
  }
  if (parts.length > MAX_STEPS) {
    throw new Error(`the step list has ${parts.length} steps; the limit is ${MAX_STEPS}`);
  }
  parts.forEach((part, index) => {
    const match = /^(\S+)\s*(.*)$/s.exec(part);
    const verb = match[1].toLowerCase();
    const rule = VERBS[verb];
    if (!rule) {
      throw new Error(`step ${index + 1}: unknown verb ${clip(match[1], 40)}; use ${Object.keys(VERBS).join(', ')}`);
    }
    const rest = match[2].trim();
    const args = rest === '' ? [] : rest.split('|').map((arg) => decode(arg.trim()));
    if (args.length < rule.min || args.length > rule.max) {
      throw new Error(`step ${index + 1}: ${verb} takes ${rule.min === rule.max ? rule.min : `${rule.min} to ${rule.max}`} argument(s), got ${args.length}`);
    }
    steps.push({ verb, args, text: part });
  });
  return steps;
}

function target(page, selector) {
  if (/^e\d+$/.test(selector)) {
    if (!hasSnapshot) {
      throw new Error(`element ref ${selector} needs a snapshot step earlier in the list`);
    }
    return page.locator(`aria-ref=${selector}`);
  }
  return page.locator(selector);
}

function resolve(arg) {
  const relative = arg.replace(/^\/+/, '');
  const base = new URL(config.base + '/');
  const url = new URL(relative === '' || relative === '.' ? '' : relative, base);
  if (url.origin !== base.origin) {
    throw new Error(`${clip(arg, 80)} leaves the service base URL`);
  }
  return url.toString();
}

function writeEvidence(relative, data) {
  const file = path.join(config.out, relative);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, data);
}

function logText(entries, empty) {
  if (entries.length === 0) {
    return empty + '\n';
  }
  const shown = entries.slice(0, MAX_LOG_LINES);
  if (entries.length > MAX_LOG_LINES) {
    shown.push(`... ${entries.length - MAX_LOG_LINES} more entries omitted`);
  }
  return shown.join('\n') + '\n';
}

async function snapshotText(page) {
  let body;
  if (typeof page._snapshotForAI === 'function') {
    const result = await page._snapshotForAI();
    body = typeof result === 'string' ? result : result.full || result.snapshot || '';
    hasSnapshot = true;
  } else {
    body = await page.locator('body').ariaSnapshot();
  }
  const title = await page.title();
  return `# url ${page.url()}\n# title ${clip(title, 200)}\n${body}\n`;
}

async function screenshot(page, relative, full) {
  const file = path.join(config.out, relative);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  await page.screenshot({ path: file, fullPage: full });
  if (fs.statSync(file).size > MAX_IMAGE_BYTES) {
    await page.screenshot({ path: file, fullPage: false });
    return full ? ' (page was too large for a full-page capture; captured the viewport)' : '';
  }
  return '';
}

async function perform(page, step, number) {
  const tag = String(number).padStart(2, '0');
  switch (step.verb) {
    case 'goto': {
      const url = resolve(step.args[0] || '');
      const response = await page.goto(url, { waitUntil: 'load', timeout: config.timeout });
      const status = response ? response.status() : 0;
      if (status >= 400) {
        return { ok: false, detail: `${url} answered HTTP ${status}`, evidence: [] };
      }
      return { ok: true, detail: `loaded ${page.url()} with HTTP ${status || 'no response'}`, evidence: [] };
    }
    case 'click': {
      await target(page, step.args[0]).click({ timeout: config.timeout });
      await page.waitForLoadState('load', { timeout: config.timeout }).catch(() => {});
      return { ok: true, detail: `clicked ${clip(step.args[0], 120)}; page is at ${page.url()}`, evidence: [] };
    }
    case 'fill': {
      await target(page, step.args[0]).fill(step.args[1], { timeout: config.timeout });
      return { ok: true, detail: `filled ${clip(step.args[0], 120)} with ${step.args[1].length} characters`, evidence: [] };
    }
    case 'press': {
      if (step.args.length === 2) {
        await target(page, step.args[0]).press(step.args[1], { timeout: config.timeout });
      } else {
        await page.keyboard.press(step.args[0]);
      }
      await page.waitForLoadState('load', { timeout: config.timeout }).catch(() => {});
      return { ok: true, detail: `pressed ${clip(step.args[step.args.length - 1], 60)}; page is at ${page.url()}`, evidence: [] };
    }
    case 'snapshot': {
      const relative = `snapshots/${tag}-snapshot.yml`;
      const text = await snapshotText(page);
      writeEvidence(relative, text);
      const refs = (text.match(/\[ref=e\d+\]/g) || []).length;
      return { ok: true, detail: `accessibility snapshot of ${page.url()} with ${refs} element refs`, evidence: [relative] };
    }
    case 'screenshot': {
      const relative = `screenshots/${tag}-screenshot.png`;
      const full = (step.args[0] || '').toLowerCase() === 'full';
      const note = await screenshot(page, relative, full);
      return { ok: true, detail: `screenshot of ${page.url()}${note}`, evidence: [relative] };
    }
    case 'console': {
      const relative = `console/${tag}-console.log`;
      writeEvidence(relative, logText(consoleLog, 'no console messages'));
      const detail = `${consoleLog.length} console messages, ${consoleErrors} errors`;
      return { ok: consoleErrors === 0, detail, evidence: [relative] };
    }
    case 'requests': {
      const relative = `requests/${tag}-requests.log`;
      writeEvidence(relative, logText(requestLog, 'no requests'));
      const detail = `${requestLog.length} requests, ${requestProblems} failed or answered HTTP 500 or above`;
      return { ok: requestProblems === 0, detail, evidence: [relative] };
    }
  }
  throw new Error(`unhandled verb ${step.verb}`);
}

function claim(name, status, detail, evidence) {
  const entry = { name, status, detail: clip(detail, MAX_DETAIL), evidence: evidence.concat(['log.txt']) };
  if (config.flow) {
    entry.flow = config.flow;
  }
  claims.push(entry);
}

function watch(page) {
  page.on('console', (message) => {
    const type = message.type();
    if (type === 'error') {
      consoleErrors++;
    }
    consoleLog.push(`[${type}] ${message.text().replace(/\s+/g, ' ')}`);
  });
  page.on('pageerror', (error) => {
    consoleErrors++;
    consoleLog.push(`[pageerror] ${clip(error.message, 300)}`);
  });
  page.on('requestfailed', (request) => {
    requestProblems++;
    requestLog.push(`${request.method()} ${request.url()} failed ${request.failure() ? request.failure().errorText : ''}`.trim());
  });
  page.on('response', (response) => {
    const status = response.status();
    if (status >= 500) {
      requestProblems++;
    }
    requestLog.push(`${response.request().method()} ${response.url()} ${status} ${response.request().resourceType()}`);
  });
}

function finish() {
  writeEvidence('log.txt', lines.join('\n') + '\n');
  writeEvidence('report.json', JSON.stringify({ version: 1, claims }, null, 2) + '\n');
}

async function main() {
  fs.mkdirSync(config.out, { recursive: true });
  say(`base url ${config.base}`);
  say(`flow ${config.flow || '(none)'}`);
  let steps;
  try {
    steps = parseSteps(config.steps);
  } catch (error) {
    say(`step list refused: ${error.message}`);
    claim('step list', 'inconclusive', `no step ran: ${error.message}`, []);
    return;
  }
  let browser;
  try {
    browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  } catch (error) {
    say(`browser did not start: ${error.message}`);
    claim('browser', 'inconclusive', `no step ran: Chromium did not start: ${clip(error.message, 300)}`, []);
    return;
  }
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    const page = await context.newPage();
    page.setDefaultTimeout(config.timeout);
    watch(page);
    let failed = null;
    for (let index = 0; index < steps.length; index++) {
      const step = steps[index];
      const number = index + 1;
      const name = `step ${String(number).padStart(2, '0')} ${clip(step.text, 100)}`;
      if (failed) {
        say(`${name}: skipped because step ${failed} failed`);
        claim(name, 'inconclusive', `not run because step ${failed} failed`, []);
        continue;
      }
      try {
        const result = await perform(page, step, number);
        say(`${name}: ${result.ok ? 'pass' : 'fail'}: ${result.detail}`);
        claim(name, result.ok ? 'pass' : 'fail', result.detail, result.evidence);
        if (!result.ok) {
          failed = number;
        }
      } catch (error) {
        const detail = clip(String(error.message || error).split('\n')[0], MAX_DETAIL);
        say(`${name}: fail: ${detail}`);
        const evidence = [];
        try {
          const relative = `screenshots/${String(number).padStart(2, '0')}-failure.png`;
          await screenshot(page, relative, false);
          evidence.push(relative);
        } catch (shotError) {
          say(`${name}: no failure screenshot: ${clip(shotError.message, 200)}`);
        }
        claim(name, 'fail', detail, evidence);
        failed = number;
      }
    }
  } finally {
    await browser.close().catch(() => {});
  }
}

main()
  .catch((error) => {
    say(`driver error: ${error.stack || error}`);
    if (claims.length === 0) {
      claim('driver', 'inconclusive', `the driver stopped early: ${clip(error.message || error, 300)}`, []);
    }
  })
  .finally(() => {
    finish();
  });
