#!/usr/bin/env python3
"""Merge JSON objects, later files winning.

Used by the checks to build a registration request out of a manifest and the
signature over it. A shell one-liner doing this is a quoting problem waiting to
happen, and the checks are supposed to be the thing that catches problems.
"""

import json
import sys

merged: dict[str, object] = {}
for path in sys.argv[1:]:
    with open(path) as handle:
        merged |= json.load(handle)
json.dump(merged, sys.stdout)
