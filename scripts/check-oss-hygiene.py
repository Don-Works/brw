#!/usr/bin/env python3

import json
import os
import re
import subprocess
import sys

ARGS = [a for a in sys.argv[1:] if a != "--full"]
FULL = "--full" in sys.argv[1:]
BASE = ARGS[0] if ARGS else "origin/main"
SELF = "scripts/check-oss-hygiene.py"


PUBLIC_SYNTHETIC_FIXTURES = {
    SELF,
    "scripts/test-functional.sh",
    "internal/recipe/schema_test.go",
    "internal/recipe/assertions_test.go",
}


SYNTHETIC_HOME_FIXTURES = {
    "cmd/brwctl/setup_test.go",
    "internal/setup/service_test.go",
    "internal/setup/skills_test.go",
    "internal/setup/claudechrome_test.go",
}


SYNTHETIC_VECTOR_WAIVERS = {
    "internal/http/server_guard_test.go": {"tailscale host"},
    "internal/browser/upload_test.go": {"private network address"},
    "internal/extensionbridge/bridge_release_conn_test.go": {"credential literal"},
    "cmd/brwctl/main_test.go": {"personal email"},
    "packaging/linux/nfpm.yaml": {"personal email"},
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


    ("personal email", r"[a-zA-Z0-9._%+-]+@(?!example\.(com|org|net|edu)\b)(?![a-zA-Z0-9.-]*\.(test|invalid|example|localhost)\b)[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}"),
    ("local workspace or profile name", r"brw-chromium-work|chromium-work-profile"),
    ("launchd label", r"co\.revitt\."),


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

    proc = subprocess.run(["git"] + args, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.exit(f"hygiene scan could not {what}: git {' '.join(args)}\n{proc.stderr.strip()}")
    return proc.stdout


def added_lines(base):

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

    if isinstance(value, dict):
        recipe_keys = {"schema_version", "id", "version", "origins", "steps"}
        if recipe_keys.issubset(value) and isinstance(value.get("steps"), list):
            if any(isinstance(step, dict) and "action" in step for step in value["steps"]):
                return True
        return any(contains_recipe_document(item) for item in value.values())
    if isinstance(value, list):
        return any(contains_recipe_document(item) for item in value)
    return False


NON_ROUTABLE_EMAIL = re.compile(r"(?i)^no-?reply@|@([a-z0-9.-]*\.)?noreply\.[a-z0-9.-]+$")


def is_non_routable_email(address):

    return bool(NON_ROUTABLE_EMAIL.search(address))


def commit_messages(base):

    revs = git(["log", f"{base}..HEAD", "--format=%H"], f"list commits since {base}").split()
    for rev in revs:
        body = git(["log", "-1", "--format=%B", rev], f"read commit message {rev[:12]}")
        for line in body.splitlines():
            yield f"commit {rev[:12]}", line


def main():
    hits = []


    repository_paths = set(git(["ls-files"], "list tracked paths").splitlines())
    repository_paths.update(git(["ls-files", "--others", "--exclude-standard"], "list untracked paths").splitlines())
    for repository_path in sorted(repository_paths):
        if RECIPE_CORPUS_PATH.search(repository_path):
            hits.append(("repository recipe corpus", repository_path, "private recipes must live outside the brw repository"))
        try:


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


    for ref, line in commit_messages(BASE):
        scanned += 1
        for label, pattern in PATTERNS:
            match = re.search(pattern, line)
            if match and not (label == "personal email" and is_non_routable_email(match.group(0))):
                hits.append((label, ref, line.strip()[:160]))
    for path, line in (tracked_lines() if FULL else added_lines(BASE)):


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
