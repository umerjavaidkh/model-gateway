"""Run the sidecar on a Unix socket.

A socket rather than a port: the sidecar runs in the same pod as the worker and
must not be reachable from anywhere else. A localhost port is reachable by every
other container in the pod and, with a misconfiguration, from outside it — for a
service whose entire input is unredacted personal data, that is the wrong
default to leave available.
"""

from __future__ import annotations

import os
from pathlib import Path

import uvicorn

from pii_ner.app import create_app
from pii_ner.recognizers import Registry

#: A default for local runs only; a deployment sets PII_NER_SOCKET to a path
#: inside the pod's own volume.
DEFAULT_SOCKET = "/tmp/pii-ner.sock"


def main() -> int:
    """Start the sidecar. Returns a process exit code."""
    socket_path = Path(os.environ.get("PII_NER_SOCKET", DEFAULT_SOCKET))

    # A socket left behind by a crashed process would make bind fail, and the
    # pod would crash-loop for a reason that has nothing to do with the code.
    socket_path.unlink(missing_ok=True)
    socket_path.parent.mkdir(parents=True, exist_ok=True)

    app = create_app(Registry(), backend=os.environ.get("PII_NER_BACKEND", "patterns"))
    uvicorn.run(app, uds=str(socket_path), log_level="info")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
