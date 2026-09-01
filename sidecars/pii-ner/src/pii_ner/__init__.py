"""Statistical PII detection, out of process.

# Why this is a sidecar and not a library

The models are Python, they carry their own dependency and CVE surface, they
want to scale independently of the request path, and they take tens of
milliseconds — an order of magnitude more than everything else in the hot path
combined.

Running them behind a Unix socket on the same pod costs roughly 0.2 ms of IPC
and buys all of that separation. The gateway calls this only when policy asks,
so the cost is paid by the requests that need it and no others.

# Why the registry is per language

An English NER model misses Arabic entities almost entirely. A single
recogniser set is therefore not a smaller version of the right answer — it is
the wrong answer for every tenant whose traffic is not in English, and it fails
silently, which is the worst way for a detector to be wrong.
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
