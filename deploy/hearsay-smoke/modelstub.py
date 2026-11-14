#!/usr/bin/env python3
"""A local stand-in for the model provider of Hearsay's distill and assert tiers.

It answers the Anthropic Messages API as those tiers call it: a one-line summary
for a distill call and no stances for an assert call. It is the only thing the
Compose smoke run replaces in Hearsay; a decision is placed from its structured
fields with no model call, so the stub also fails the run's check that no
decision text reached it by logging every prompt it receives.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200, {"ok": True})

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length", "0")))
        try:
            req = json.loads(raw)
            tool = req["tools"][0]
            text = req["messages"][0]["content"]
            if not isinstance(text, str):
                text = json.dumps(text)
            props = tool["input_schema"]["properties"]
        except (KeyError, IndexError, ValueError, TypeError):
            return self.reply(400, {"error": {"type": "invalid_request_error", "message": "the stub answers structured Messages calls only"}})
        if self.path != "/v1/messages":
            return self.reply(400, {"error": {"type": "invalid_request_error", "message": "unknown path"}})
        print(json.dumps({"prompt": text}), flush=True)
        if "assertions" in props:
            answer = {"assertions": []}
        elif "summary" in props:
            answer = {"summary": text.strip().split("\n", 1)[0], "outcome_kind": "none"}
        else:
            return self.reply(400, {"error": {"type": "invalid_request_error", "message": "the stub does not know this schema"}})
        self.reply(200, {
            "id": "msg_stub", "type": "message", "role": "assistant", "model": "stub", "stop_reason": "tool_use",
            "content": [{"type": "tool_use", "id": "toolu_stub", "name": tool["name"], "input": answer}],
            "usage": {"input_tokens": 1, "output_tokens": 1},
        })


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8090
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
