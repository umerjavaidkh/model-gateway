#!/usr/bin/env python3
"""A minimal third-party guardrail, as a component publisher would write one.

It exists so the admission path can be exercised end to end without inventing a
container image: it speaks the sidecar protocol over the socket the sandbox
hands it, and it is the smallest thing that can legitimately pass the guardrail
contract suite.

Set STUB_BEHAVIOUR to make it misbehave, which is how the check proves the
suite actually catches things rather than passing everything:

    conforming  (default) denies payloads containing AKIA, allows the rest
    deny-all    refuses everything, including benign traffic
    allow-all   allows everything, including what it claims to catch
"""

from __future__ import annotations

import base64
import json
import os
import socketserver
import sys
from http.server import BaseHTTPRequestHandler

BEHAVIOUR = os.environ.get("STUB_BEHAVIOUR", "conforming")


def verdict_for(payload: bytes) -> dict[str, object]:
    if BEHAVIOUR == "deny-all":
        return {"verdict": "deny", "reason": "everything is suspicious"}
    if BEHAVIOUR == "allow-all":
        return {"verdict": "allow"}
    if b"AKIA" in payload:
        return {"verdict": "deny", "reason": "aws-access-key"}
    return {"verdict": "allow"}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _reply(self, body: dict[str, object]) -> None:
        encoded = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self) -> None:  # noqa: N802 - the base class names it
        self._reply({"status": "ok"})

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(length) or b"{}")
        # []byte is base64 in Go's JSON, which is the part a component in
        # another language has to get right.
        payload = base64.b64decode(request.get("payload") or "")
        self._reply(verdict_for(payload))

    def log_message(self, *_: object) -> None:
        pass


class Server(socketserver.ThreadingUnixStreamServer):
    allow_reuse_address = True

    def get_request(self) -> tuple[object, tuple[str, int]]:
        # BaseHTTPRequestHandler expects a (host, port) client address; a Unix
        # socket has neither, so one is invented for its logging.
        request, _ = super().get_request()
        return request, ("local", 0)


def main() -> int:
    path = os.environ.get("COMPONENT_SOCKET")
    if not path:
        print("COMPONENT_SOCKET is not set", file=sys.stderr)
        return 1

    with Server(path, Handler) as server:
        # No chmod here. A component running under a different uid from the
        # runner has to make its socket connectable, but widening the mode is
        # its call to make in its own image — an example that ships a
        # world-writable socket is one people copy.
        server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
