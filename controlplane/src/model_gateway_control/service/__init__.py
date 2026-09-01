"""Application services: the operations the admin API exposes.

Kept separate from the HTTP layer so that a route handler is only translation —
parse, call, serialise — and every rule about what an operation means lives
somewhere that can be tested without a server.
"""
