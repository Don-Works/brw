#!/usr/bin/env python3
"""Open-source hygiene gate for the public tree and lines this release adds.

brw is public. Nothing committed may carry personal data, secrets, local machine
paths, profile or workspace names, deployment topology, or internal operating
detail. Verification against a local signed-in browser informs the work; it must
not appear in the repo.

Executable recipe-shaped JSON and recipe-corpus paths are rejected across the
entire tracked and untracked tree. PII and secret patterns are scanned in ADDED
lines against a base ref; re-flagging already-released public install paths on
every run would train people to ignore the scanner.

COMMIT MESSAGES are scanned with the same patterns. A message is committed and
published exactly like a file is, but it appears in no diff, so scanning only
added lines let an operator machine name reach the public history of this repo.

`--full` scans EVERY tracked file instead of added lines only. The default is
deliberately incremental so CI stays fast and so an already-published install
path is not re-reported on every run, but that also means anything committed
before its rule existed is grandfathered in forever and no build will ever
mention it. Run the full scan before a release to see that backlog.
"""
import json
import os
import re
import subprocess
import sys

ARGS = [a for a in sys.argv[1:] if a != "--full"]
FULL = "--full" in sys.argv[1:]
BASE = ARGS[0] if ARGS else "origin/main"
SELF = "scripts/check-oss-hygiene.py"
# Schema tests have to carry a whole recipe document as literal JSON: the parser
# rejects unknown fields, so a Go struct round-trip would not prove the published
# field names. Every recipe in these files is fabricated and targets example.test.
PUBLIC_SYNTHETIC_FIXTURES = {
    SELF,
    "scripts/test-functional.sh",
    "internal/recipe/schema_test.go",
    "internal/recipe/assertions_test.go",
}

# Setup renders absolute LaunchAgent, systemd, log and policy paths, so its tests
# have to assert on home-shaped literals for a fabricated user. Only the home
# path rule is waived for these files; an email, token, key, tailnet host or
# private address in one is still a finding.
SYNTHETIC_HOME_FIXTURES = {
    "cmd/brwctl/setup_test.go",
    "internal/setup/service_test.go",
    "internal/setup/skills_test.go",
    "internal/setup/claudechrome_test.go",
}

# Deliberately fabricated test vectors. Without these, --full drowns a real
# finding in intentional fixture data and stops being read. Each entry waives
# ONLY the named rule for that path; every other rule still applies to it.
SYNTHETIC_VECTOR_WAIVERS = {
    "internal/http/server_guard_test.go": {"tailscale host"},
    "internal/browser/upload_test.go": {"private network address"},
    "internal/extensionbridge/bridge_release_conn_test.go": {"credential literal"},
    "cmd/brwctl/main_test.go": {"personal email"},  # aes256-gcm@openssh.com is a cipher
    "packaging/linux/nfpm.yaml": {"personal email"},  # package maintainer contact
}

RECIPE_CORPUS_PATH = re.compile(
    r"(^|/)(recipes|private-recipes|recipe-bank|recipe-cache)(/|$)|\.recipe\.json$",
    re.IGNORECASE,
)

BINARY_SUFFIXES = (
    ".png", ".jpg", ".jpeg", ".ico", ".gz", ".zip", ".webm", ".mp4",
    ".pdf", ".woff", ".woff2", ".ttf", ".icns", ".wasm",
)

PATTERNS = [
    ("home directory path", r"/Users/[a-z]|/home/[a-z]"),
    # The reserved names are RFC 2606 / RFC 6761: they can never route to a real
    # mailbox, so a fixture using one is not a leak. Everything else is.
    ("personal email", r"[a-zA-Z0-9._%+-]+@(?!example\.(com|org|net|edu)\b)(?![a-zA-Z0-9.-]*\.(test|invalid|example|localhost)\b)[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}"),
    ("local workspace or profile name", r"brw-chromium-work|chromium-work-profile"),
    ("launchd label", r"co\.revitt\."),
    # Bare operator hostnames match none of the rules above: they carry no
    # domain, no path and no label prefix. They still name a specific machine.
    ("operator machine name", r"\bmax-(mac|air)\b"),
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


def tracked_lines():
    """Yield (path, line) for every line of every tracked text file.

    This is what --full scans. It reports the backlog the incremental default
    can never surface: a string committed before the rule that would flag it."""
    for path in git(["ls-files"], "list tracked paths").splitlines():
        if path == SELF or path.endswith(BINARY_SUFFIXES):
            continue
        try:
            if os.path.getsize(path) > 2 << 20:
                continue
            with open(path, encoding="utf-8", errors="ignore") as fh:
                for line in fh:
                    yield path, line.rstrip("\n")
        except (FileNotFoundError, IsADirectoryError, OSError):
            continue


def contains_recipe_document(value):
    """Recognize the executable ABI even when a corpus file has an innocent
    name. Nested JSON is checked because a bundle may contain many recipes."""
    if isinstance(value, dict):
        recipe_keys = {"schema_version", "id", "version", "origins", "steps"}
        if recipe_keys.issubset(value) and isinstance(value.get("steps"), list):
            if any(isinstance(step, dict) and "action" in step for step in value["steps"]):
                return True
        return any(contains_recipe_document(item) for item in value.values())
    if isinstance(value, list):
        return any(contains_recipe_document(item) for item in value)
    return False


# noreply forms cannot receive mail, so they are not a contact-detail leak.
NON_ROUTABLE_EMAIL = re.compile(r"(?i)^no-?reply@|@([a-z0-9.-]*\.)?noreply\.[a-z0-9.-]+$")


def is_non_routable_email(address):
    """Report whether an address can never reach a mailbox.

    Co-authorship and sign-off trailers are structured metadata that every commit
    here carries, and the addresses in them are the provider's noreply forms.
    Flagging those would fail every commit and train people to skip the gate —
    the same reasoning that already exempts the RFC 2606 reserved domains. A real
    address in a trailer is still a finding."""
    return bool(NON_ROUTABLE_EMAIL.search(address))


def commit_messages(base):
    """Yield (ref, line) for every line of every commit message added over base.

    A commit message is published as surely as a file is, and the docstring above
    promises nothing committed carries local machine detail. added_lines() reads
    diffs, and a message appears in no diff, so without this a hostname in a
    commit message reached the public history unchallenged."""
    revs = git(["log", f"{base}..HEAD", "--format=%H"], f"list commits since {base}").split()
    for rev in revs:
        body = git(["log", "-1", "--format=%B", rev], f"read commit message {rev[:12]}")
        for line in body.splitlines():
            yield f"commit {rev[:12]}", line


def main():
    hits = []
    # Public brw owns the recipe ABI, never an operator's executable corpus.
    # Check every tracked path (not just added lines) so a rename or merge cannot
    # quietly bypass the content-oriented scanner below.
    repository_paths = set(git(["ls-files"], "list tracked paths").splitlines())
    repository_paths.update(git(["ls-files", "--others", "--exclude-standard"], "list untracked paths").splitlines())
    for repository_path in sorted(repository_paths):
        if RECIPE_CORPUS_PATH.search(repository_path):
            hits.append(("repository recipe corpus", repository_path, "private recipes must live outside the brw repository"))
        try:
            # The runtime rejects recipes over 1 MiB. Inspect any plausibly JSON
            # text file up to twice that size so renaming a corpus item to .txt
            # or another innocent extension cannot bypass the gate.
            if os.path.getsize(repository_path) > 2 << 20:
                continue
            with open(repository_path, encoding="utf-8") as fh:
                raw = fh.read()
            embedded_markers = (
                '"schema_version":', '"origins":', '"steps":', '"action":'
            )
            if (
                repository_path not in PUBLIC_SYNTHETIC_FIXTURES
                and all(marker in raw for marker in embedded_markers)
            ):
                hits.append((
                    "embedded executable recipe",
                    repository_path,
                    "move operational recipe bodies outside the public tree",
                ))
            if not raw.lstrip().startswith(("{", "[")):
                continue
            value = json.loads(raw)
        except (FileNotFoundError, IsADirectoryError, OSError, UnicodeDecodeError, json.JSONDecodeError):
            continue
        if contains_recipe_document(value):
            hits.append(("executable recipe JSON", repository_path, "move operational recipes to --recipe-root or the private provider"))
    scanned = 0
    # Commit messages are scanned with the same patterns, but never with the
    # per-file waivers: a synthetic-home fixture is a file, not a message.
    for ref, line in commit_messages(BASE):
        scanned += 1
        for label, pattern in PATTERNS:
            match = re.search(pattern, line)
            if match and not (label == "personal email" and is_non_routable_email(match.group(0))):
                hits.append((label, ref, line.strip()[:160]))
    for path, line in (tracked_lines() if FULL else added_lines(BASE)):
        # A scanner cannot scan its own rules: the patterns necessarily contain
        # the very strings they look for.
        if path.startswith(".scratch") or path == SELF:
            continue
        scanned += 1
        for label, pattern in PATTERNS:
            if label == "home directory path" and path in SYNTHETIC_HOME_FIXTURES:
                continue
            if label in SYNTHETIC_VECTOR_WAIVERS.get(path, ()):
                continue
            if re.search(pattern, line):
                hits.append((label, path, line.strip()[:160]))

    scope = "tracked lines (full tree)" if FULL else "added lines"
    if not hits:
        print(f"clean: {scanned} {scope} carry no PII, secrets, or local-only detail")
        return
    print(f"{len(hits)} issue(s) in {scope}:\n")
    for label, path, text in hits:
        print(f"  [{label}] {path}\n      {text}")
    sys.exit(1)


if __name__ == "__main__":
    main()
