"""The admin API.

Route handlers here are translation only — parse, call a service, serialise.
Every rule about what an operation *means* lives in ``service/``, so it can be
tested without a server and reused by a second entry point.

# Listener

This is a separate application from the data plane and is expected to run on its
own listener behind mTLS. mTLS is deployment configuration rather than code; the
bearer token checked here is the in-process half of that, and is not a substitute
for it. An admin API reachable from the internet with only a bearer token in
front of it is the shape of every gateway CVE in the plan's §1.
"""

from model_gateway_control.api.app import create_app

__all__ = ["create_app"]
