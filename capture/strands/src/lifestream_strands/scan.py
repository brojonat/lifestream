"""Deterministic secret scanner: the Python twin of internal/scan. Same
patterns in the same order, same [REDACTED:<kind>] masks, pinned by the same
corpus (testdata/secrets.json)."""

import re
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class Pattern:
    kind: str
    re: re.Pattern[str]
    group: int = 0  # the capture group holding the secret; 0 masks the whole match


# Multi-line and prefixed tokens first, so later generic rules see the mask.
PATTERNS = [
    Pattern("private_key", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----")),
    Pattern("aws_access_key", re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b")),
    Pattern("github_token", re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})")),
    Pattern("anthropic_key", re.compile(r"\bsk-ant-[A-Za-z0-9_-]{20,}")),
    Pattern("openai_key", re.compile(r"\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}")),
    Pattern("stripe_key", re.compile(r"\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}")),
    Pattern("slack_token", re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}")),
    Pattern("google_api_key", re.compile(r"\bAIza[0-9A-Za-z_-]{35}")),
    Pattern("twilio_sid", re.compile(r"\b(?:AC|SK)[0-9a-f]{32}\b")),
    Pattern("jwt", re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{10,}")),
    Pattern("bearer", re.compile(r"\b[Bb]earer\s+([A-Za-z0-9\-._~+/]{20,}=*)"), 1),
    Pattern("connection_password", re.compile(r"\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@"), 1),
    Pattern(
        "assigned_secret",
        re.compile(
            r"""\b(?:[A-Za-z0-9]+[_-])?(?:api[_-]?key|secret|token|passw(?:or)?d|pwd)["']?\s*[:=]\s*["']?([^\s"',;]{8,})""", re.IGNORECASE
        ),
        1,
    ),
    Pattern("email", re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")),
    Pattern("phone", re.compile(r"(?<![\w+])\+[1-9]\d{7,14}\b")),
]


def _mask(kind: str) -> str:
    return f"[REDACTED:{kind}]"


def scan_text(text: str) -> tuple[str, dict[str, int]]:
    """Redact one string; returns the text and a count per kind."""
    counts: dict[str, int] = {}
    for p in PATTERNS:

        def sub(m: re.Match[str], p: Pattern = p) -> str:
            secret = m.group(p.group)
            # Never match across an earlier mask (a token inside a URL, say).
            if not secret or "[REDACTED:" in m.group(0):
                return m.group(0)
            counts[p.kind] = counts.get(p.kind, 0) + 1
            if p.group == 0:
                return _mask(p.kind)
            start, end = m.start(p.group) - m.start(), m.end(p.group) - m.start()
            whole = m.group(0)
            return whole[:start] + _mask(p.kind) + whole[end:]

        text = p.re.sub(sub, text)
    return text, counts


def scan_json(value: Any) -> tuple[Any, dict[str, int]]:
    """Redact every string in a JSON-like value (keys stay); returns a copy."""
    counts: dict[str, int] = {}

    def walk(v: Any) -> Any:
        if isinstance(v, str):
            out, found = scan_text(v)
            for k, n in found.items():
                counts[k] = counts.get(k, 0) + n
            return out
        if isinstance(v, list | tuple):
            return [walk(x) for x in v]
        if isinstance(v, dict):
            return {k: walk(x) for k, x in v.items()}
        return v

    return walk(value), counts
