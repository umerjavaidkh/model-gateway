#!/usr/bin/env python3
"""Generate REFERENCE.md from the code.

Generated, not written. A hand-maintained index of endpoints and modules is
wrong the first time somebody adds one and forgets, and a reference that might
be wrong is worse than none — you stop checking the code and start trusting it.

This session produced a concrete example: an endpoint list assembled by grep
missed every route whose decorator wrapped across two lines, and a confident,
wrong conclusion was drawn from it. Reading the routes out of the application
object cannot miss one.

Run `make reference`. CI checks the file is current, the same way it checks
generated protobuf.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "REFERENCE.md"


def admin_routes() -> list[tuple[str, str]]:
    """Every admin API route, from the application itself."""
    script = """
import json, os
os.environ.setdefault("GATEWAY_DATABASE_URL", "sqlite+aiosqlite:///:memory:")
from model_gateway_control.api.app import AdminSettings, create_app
from model_gateway_control.db.session import create_engine

app = create_app(AdminSettings(
    engine=create_engine("sqlite+aiosqlite:///:memory:"),
    key_pepper=b"x" * 32,
    admin_token="y" * 32,
))
routes = []
DOCS = {"/docs", "/redoc", "/openapi.json", "/docs/oauth2-redirect"}
for route in app.routes:
    methods = getattr(route, "methods", None)
    # FastAPI's own documentation routes are not part of the surface anyone
    # integrates against.
    if not methods or route.path in DOCS:
        continue
    for method in sorted(methods - {"HEAD", "OPTIONS"}):
        routes.append([method, route.path])
print(json.dumps(sorted(routes, key=lambda r: (r[1], r[0]))))
"""
    result = subprocess.run(
        ["uv", "run", "python", "-c", script],
        cwd=ROOT / "controlplane",
        capture_output=True,
        text=True,
        check=True,
    )
    import json

    return [(m, p) for m, p in json.loads(result.stdout.strip().splitlines()[-1])]


def go_packages() -> list[tuple[str, str]]:
    """Data-plane packages and the first line of their doc comment."""
    packages = []
    for directory in sorted((ROOT / "dataplane" / "internal").rglob("*")):
        if not directory.is_dir() or directory.name == "testdata":
            continue
        for source in sorted(directory.glob("*.go")):
            if source.name.endswith("_test.go"):
                continue
            text = source.read_text()
            match = re.search(r"^// Package (\w+) (.+?)$", text, re.MULTILINE)
            if match:
                rel = directory.relative_to(ROOT / "dataplane")
                packages.append((str(rel), match.group(2).rstrip(".")))
                break
    return packages


def python_modules() -> list[tuple[str, str]]:
    """Control-plane modules and the first line of their docstring."""
    base = ROOT / "controlplane" / "src" / "model_gateway_control"
    modules = []
    for source in sorted(base.rglob("*.py")):
        if "migrations" in source.parts or "wire" in source.parts:
            continue
        if source.name == "__init__.py":
            continue
        match = re.match(r'^"""(.+?)$', source.read_text(), re.MULTILINE)
        if match:
            modules.append((str(source.relative_to(base)), match.group(1).rstrip(".")))
    return modules


def adrs() -> list[tuple[str, str]]:
    """Every decision record, by title."""
    records = []
    for path in sorted((ROOT / "docs" / "adr").glob("*.md")):
        first = path.read_text().splitlines()[0].lstrip("# ").strip()
        records.append((path.name, first))
    return records


def modules_table() -> str:
    """The module status table, lifted from the README so there is one copy."""
    readme = (ROOT / "README.md").read_text()
    # Taken line by line rather than with one regex: a table is "the header
    # and every row after it", and expressing that as a lookahead is how it
    # breaks the day someone adds a trailing section.
    lines = readme.splitlines()
    for i, line in enumerate(lines):
        if line.startswith("| Module ") and "State" in line:
            table = [line]
            for row in lines[i + 1 :]:
                if not row.startswith("|"):
                    break
                table.append(row)
            return "\n".join(table)
    return "_not found in README.md_"


def env_vars() -> list[tuple[str, str]]:
    """Environment variables the binaries read."""
    # Every reader, not the first one found. A variable read in four places
    # attributed to one of them reads as "this is where it is configured",
    # which is exactly the wrong impression for something like the database URL.
    found: dict[str, set[str]] = {}
    # Sorted, because rglob returns filesystem order and a variable read in two
    # files would otherwise be attributed to whichever the machine happened to
    # walk first. That made this file differ between a laptop and CI by one
    # line, which is exactly the drift the check exists to catch — pointed at
    # the generator rather than at anyone's edit.
    sources = sorted((ROOT / "dataplane").rglob("*.go")) + sorted(
        (ROOT / "controlplane" / "src").rglob("*.py")
    )
    for path in sources:
        if path.name.endswith("_test.go") or "/tests/" in str(path):
            continue
        text = path.read_text()
        pattern = r'(?:getenv|environ\.get)\(\s*"(GATEWAY_[A-Z_]+)"|os\.environ\[\s*"(GATEWAY_[A-Z_]+)"\s*\]'
        for direct, indexed in re.findall(pattern, text):
            found.setdefault(direct or indexed, set()).add(path.name)
    return [(name, ", ".join(sorted(files))) for name, files in sorted(found.items())]


def main() -> int:
    lines = [
        "# Reference",
        "",
        "**Generated by `make reference`. Do not edit.**",
        "",
        "A hand-maintained index is wrong the first time somebody adds an endpoint",
        "and forgets, and a reference that might be wrong is worse than none — you",
        "stop checking the code and start trusting it. Everything below is read out",
        "of the code, so it cannot drift without the check in CI failing.",
        "",
        "## Modules",
        "",
        modules_table(),
        "",
        "## Admin API",
        "",
        "Every route on the admin application, read from the app object rather than",
        "matched with a regex — decorators that wrap across lines are exactly how an",
        "endpoint goes missing from a list like this.",
        "",
        "| Method | Path |",
        "|---|---|",
    ]
    for method, path in admin_routes():
        lines.append(f"| `{method}` | `{path}` |")

    lines += ["", "## Data plane (Go)", "", "| Package | What it is |", "|---|---|"]
    for name, summary in go_packages():
        lines.append(f"| `{name}` | {summary} |")

    lines += ["", "## Control plane (Python)", "", "| Module | What it is |", "|---|---|"]
    for name, summary in python_modules():
        lines.append(f"| `{name}` | {summary} |")

    lines += ["", "## Configuration", "", "| Variable | Read by |", "|---|---|"]
    for name, where in env_vars():
        lines.append(f"| `{name}` | `{where}` |")

    lines += ["", "## Decisions", "", "| Record | Title |", "|---|---|"]
    for name, title in adrs():
        lines.append(f"| [`{name}`](docs/adr/{name}) | {title} |")

    lines.append("")
    OUT.write_text("\n".join(lines))
    print(f"wrote {OUT.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
