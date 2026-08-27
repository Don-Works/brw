#!/usr/bin/env python3
"""Open-source hygiene gate for the lines this release adds.

brw is public. Nothing committed may carry personal data, secrets, local machine
paths, profile or workspace names, deployment topology, or internal operating
detail. Verification against a local signed-in browser informs the work; it must
not appear in the repo.

Scans only ADDED lines, against a base ref. Content already released is out of
scope — the documented install path under Library/Application Support is public
and correct, and re-flagging it every run trains people to ignore the scanner.
"""
import re
import subprocess
import sys

BASE = sys.argv[1] if len(sys.argv) > 1 else "origin/main"
SELF = "scripts/check-oss-hygiene.py"

PATTERNS = [
    ("home directory path", r"/Users/[a-z]|/home/[a-z]"),
    ("personal email", r"[a-zA-Z0-9._%+-]+@(?!example\.(com|org)\b)[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}"),
    ("local workspace or profile name", r"brw-chromium-work|chromium-work-profile"),
    ("launchd label", r"co\.revitt\."),
    ("tailscale host", r"[a-z0-9-]+\.ts\.net"),
    ("private network address", r"\b(10|192\.168|172\.(1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}(\.\d{1,3})?\b"),
    ("credential literal", r"(?i)\b(api[_-]?key|secret|token|passwd|password|bearer)\b\s*[:=]\s*['\"][^'\"]{8,}"),
    ("aws access key", r"\bAKIA[0-9A-Z]{16}\b"),
    ("private key block", r"BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY"),
    ("github token", r"\bgh[pousr]_[A-Za-z0-9]{20,}"),
    ("slack token", r"\bxox[baprs]-[A-Za-z0-9-]{10,}"),
]


def git(args, what):
    """Run a git command, failing loudly. A gate that reports clean because its
    own scan errored is worse than no gate: a clone without origin/main would
    print "clean: 0 added lines" and exit 0 with a secret in the diff."""
    proc = subprocess.run(["git"] + args, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.exit(f"hygiene scan could not {what}: git {' '.join(args)}\n{proc.stderr.strip()}")
    return proc.stdout


def added_lines(base):
    """Yield (path, line) for every line this branch adds over base."""
    diff = git(["diff", "-U0", f"{base}...HEAD"], f"diff against {base}")
    staged = git(["diff", "-U0", "HEAD"], "diff the working tree")
    untracked = git(["ls-files", "--others", "--exclude-standard"], "list untracked files")

    path = "?"
    for chunk in (diff, staged):
        for line in chunk.splitlines():
            if line.startswith("+++ b/"):
                path = line[6:]
            elif line.startswith("+") and not line.startswith("+++"):
                yield path, line[1:]

    for new_path in untracked.splitlines():
        if new_path.startswith((".git/", "bin/", "dist/", ".claude/", ".scratch")):
            continue
        try:
            with open(new_path, encoding="utf-8", errors="ignore") as fh:
                for line in fh:
                    yield new_path, line.rstrip("\n")
        except (IsADirectoryError, FileNotFoundError):
            continue


def main():
    hits = []
    scanned = 0
    for path, line in added_lines(BASE):
        # A scanner cannot scan its own rules: the patterns necessarily contain
        # the very strings they look for.
        if path.startswith(".scratch") or path == SELF:
            continue
        scanned += 1
        for label, pattern in PATTERNS:
            if re.search(pattern, line):
                hits.append((label, path, line.strip()[:160]))

    if not hits:
        print(f"clean: {scanned} added lines carry no PII, secrets, or local-only detail")
        return
    print(f"{len(hits)} issue(s) in added lines:\n")
    for label, path, text in hits:
        print(f"  [{label}] {path}\n      {text}")
    sys.exit(1)


if __name__ == "__main__":
    main()
