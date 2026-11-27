'use strict';

const crypto = require('crypto');
const http = require('http');

const token = crypto.randomBytes(8).toString('hex');
const notes = [];
const port = Number(process.env.PORT) || 8080;

function escape(text) {
  return text.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}

function page(title, body) {
  return `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>${title}</title></head>
<body>
<header><nav><a href="/">Home</a> <a href="/notes">Notes</a></nav></header>
<main>
${body}
<p>Instance token: <code id="token">${token}</code></p>
</main>
</body>
</html>
`;
}

function home() {
  return page('Fixture home', '<h1>Verification fixture</h1>\n<p>A tiny service that the verification tests drive.</p>');
}

function notesPage() {
  const items = notes.map((note) => `<li>${escape(note)}</li>`).join('\n');
  return page('Fixture notes', `<h1>Notes</h1>
<form method="post" action="/notes">
<label for="note">Note</label>
<input id="note" name="note" type="text">
<button type="submit">Add note</button>
</form>
<ul aria-label="Saved notes">
${items}
</ul>`);
}

function send(res, status, body, headers) {
  res.writeHead(status, Object.assign({ 'content-type': 'text/html; charset=utf-8' }, headers));
  res.end(body);
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (req.method === 'GET' && url.pathname === '/') {
    return send(res, 200, home());
  }
  if (req.method === 'GET' && url.pathname === '/notes') {
    return send(res, 200, notesPage());
  }
  if (req.method === 'POST' && url.pathname === '/notes') {
    let body = '';
    req.on('data', (chunk) => {
      body += chunk;
    });
    req.on('end', () => {
      const note = (new URLSearchParams(body).get('note') || '').trim();
      if (note) {
        notes.push(note.slice(0, 200));
      }
      send(res, 303, '', { location: '/notes' });
    });
    return undefined;
  }
  return send(res, 404, page('Not found', '<h1>Not found</h1>'));
});

server.listen(port, '0.0.0.0', () => {
  console.log(`fixture listening on ${port} with instance token ${token}`);
});
