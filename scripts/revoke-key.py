#!/usr/bin/env python3
"""Mark a key revoked in a trusted-keys file, for the checks."""

import json
import sys

path, key_id = sys.argv[1], sys.argv[2]
with open(path) as handle:
    document = json.load(handle)
for key in document["keys"]:
    if key["key_id"] == key_id:
        key["status"] = "revoked"
        break
else:
    raise SystemExit(f"{path} has no key {key_id!r}")
with open(path, "w") as handle:
    json.dump(document, handle)
