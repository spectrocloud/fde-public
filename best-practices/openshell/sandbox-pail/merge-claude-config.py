#!/usr/bin/env python3
"""Merge OpenShell's provider-managed Claude settings into ~/.claude.json.

Invoked from .bashrc, so it never raises and always exits 0.

CLAUDE_CONFIG_FILE names a virtual memfd-backed file that the sandbox serves
only for open()/openat()/openat2(). Never stat()/exists() it: those syscalls
are not intercepted and report ENOENT.
"""

import copy
import json
import os
import sys
import tempfile

DEST = os.path.realpath(os.path.expanduser("~/.claude.json"))
VERBOSE = any(arg in ("-v", "--verbose") for arg in sys.argv[1:])


def log(message, *, error=False):
    if VERBOSE or error:
        print(f"merge-claude-config: {message}", file=sys.stderr)


def merge(base, incoming):
    """Deep-merge: incoming wins on conflicts, base-only keys are kept."""
    for key, value in incoming.items():
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            merge(base[key], value)
        else:
            base[key] = value
    return base


def load_destination():
    """Return the parsed destination, {} if absent, or None if unreadable."""
    try:
        with open(DEST, "r", encoding="utf-8") as handle:
            text = handle.read()
    except FileNotFoundError:
        return {}
    except OSError as error:
        log(f"cannot read {DEST}: {error}", error=True)
        return None
    if not text.strip():
        return {}
    try:
        data = json.loads(text)
    except ValueError as error:
        log(f"refusing to rewrite {DEST}: invalid JSON ({error})", error=True)
        return None
    if not isinstance(data, dict):
        log(f"refusing to rewrite {DEST}: top-level JSON is not an object", error=True)
        return None
    return data


def write_atomic(data):
    directory = os.path.dirname(DEST) or "."
    fd, tmp = tempfile.mkstemp(dir=directory, prefix=".claude.json.")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(data, handle, indent=2)
            handle.write("\n")
        if os.path.exists(DEST):
            os.chmod(tmp, os.stat(DEST).st_mode & 0o777)
        os.replace(tmp, DEST)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def main():
    source = os.environ.get("CLAUDE_CONFIG_FILE", "").strip()
    if not source:
        log("CLAUDE_CONFIG_FILE is unset or empty; nothing to do")
        return 0
    source = os.path.expanduser(source)

    try:
        with open(source, "r", encoding="utf-8") as handle:
            incoming = json.load(handle)
    except FileNotFoundError:
        log(f"{source} was not delivered; nothing to do")
        return 0
    except OSError as error:
        log(f"cannot read {source}: {error}", error=True)
        return 0
    except ValueError as error:
        log(f"{source} is not valid JSON: {error}", error=True)
        return 0
    if not isinstance(incoming, dict):
        log(f"ignoring {source}: top-level JSON is not an object", error=True)
        return 0

    base = load_destination()
    if base is None:
        return 0

    # Merge into a copy: merge() mutates in place, so comparing the result
    # against the original object would always report "unchanged".
    merged = merge(copy.deepcopy(base), incoming)
    if merged == base:
        log("already applied; not rewriting")
        return 0

    try:
        write_atomic(merged)
    except OSError as error:
        log(f"cannot write {DEST}: {error}", error=True)
        return 0
    log(f"wrote {DEST}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:  # a .bashrc hook must never break the shell
        log(f"unexpected error: {error!r}", error=True)
        sys.exit(0)