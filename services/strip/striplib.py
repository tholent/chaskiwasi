"""Core quote-stripping logic for the strip service (wasi-server-plan §11.1).

Kept separate from app.py so the golden-corpus test (test_striplib.py) can
exercise the exact same code path the HTTP handler uses, with no Flask or
network involved.

I-1: this module never logs anything. Callers (app.py) must keep it that
way — no request text, stripped or not, may reach a log line at any level.
"""

from __future__ import annotations

from dataclasses import dataclass

import talon
from talon import quotations

talon.init()


@dataclass(frozen=True)
class StripResult:
    body: str
    trimmed: bool
    removed_bytes: int


def unwrap_flowed(text: str) -> str:
    """Undoes RFC 3676 format=flowed soft-wrapping (DelSp=no, the default —
    the request contract carries a bare format_flowed bool with no DelSp, so
    that's the only mode this handles; it's also what real flowed-generating
    clients overwhelmingly use).

    A line that is "space-stuffed" (starts with a single leading space, added
    so the line doesn't get misread as a '>' quote marker or 'From ') has
    that leading space removed. A line ending in exactly one trailing space
    is a soft break: join it to the next line instead of keeping the
    newline. Per RFC 3676 §4.5, DelSp=no *keeps* that trailing space in the
    joined result — it's the word-separating space between the wrapped
    line's last word and the next line's first, not just a marker — so only
    the newline is removed, not the space. The literal signature delimiter
    "-- " is excluded from soft-break joining even though it ends in a
    space, since that space is meaningful there too.

    Talon's quote detection (the "On ... wrote:" and "> " heuristics) assumes
    normally-wrapped text; skipping this step on a flowed message leaves
    soft-break newlines and space-stuffing in place, which makes both quote
    detection and the device's own rendering (§4.9) worse for no reason.
    """
    lines = text.split("\n")
    out: list[str] = []
    for raw_line in lines:
        # A trailing \r survives if the input used CRLF; strip it here and
        # let the caller's line joining re-normalise on "\n" throughout.
        line = raw_line[:-1] if raw_line.endswith("\r") else raw_line

        if line.startswith(" "):
            line = line[1:]

        # The signature delimiter must never be merged into by the line
        # before it, or a soft-broken "...text " immediately followed by
        # "-- " would silently glue into "...text -- ".
        if out and line != "-- " and _is_soft_break(out[-1]):
            out[-1] = out[-1] + line
        else:
            out.append(line)
    return "\n".join(out)


def _is_soft_break(line: str) -> bool:
    return line.endswith(" ") and line != "-- "


def strip(text: str, format_flowed: bool) -> StripResult:
    """Runs the full pipeline: optional flowed-unwrap, then talon quote
    stripping. removed_bytes is measured in UTF-8 bytes, matching how the Go
    side (internal/strip and the sync response budget, §4.6) accounts size.
    """
    working = unwrap_flowed(text) if format_flowed else text
    stripped = quotations.extract_from_plain(working)
    if stripped is None:
        stripped = working

    # talon unconditionally normalises trailing whitespace/newlines even
    # when there was no quoted tail to find — that's not what `trimmed`
    # means on the wire (§4.3: "a quoted tail was removed by strip"), so the
    # decision and removed_bytes both ignore trailing-whitespace-only
    # differences. talon only ever cuts from the tail, never edits the
    # middle of what it keeps, so comparing rstripped lengths is exact, not
    # a heuristic.
    original_bytes = len(working.rstrip().encode("utf-8"))
    stripped_bytes = len(stripped.rstrip().encode("utf-8"))
    removed = max(0, original_bytes - stripped_bytes)

    return StripResult(
        body=stripped,
        trimmed=removed > 0,
        removed_bytes=removed,
    )
