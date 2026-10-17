"""Lifestream capture for Strands agents.

    from lifestream_strands import LifestreamCapture
    agent = Agent(hooks=[LifestreamCapture()])

Every message the agent adds (user prompts, assistant replies, tool
results) is POSTed to Lifestream as an llm.message event, as it happens.
Configuration is environment variables only: LIFESTREAM_URL,
LIFESTREAM_TOKEN, LIFESTREAM_CAPTURE (nothing is sent unless "on"),
LIFESTREAM_DENY (colon-separated path globs; a match on the working
directory or a parent turns capture off) and LIFESTREAM_SCAN ("off" sends
messages without masking secrets first).

A failed send is kept in memory and retried with the next message and at
the end of each invocation; the service drops repeats, since each event
is keyed by (session_id, sequence).
"""

import base64
import fnmatch
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
import uuid
from collections.abc import Callable, Mapping
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from strands.hooks import AfterInvocationEvent, HookProvider, HookRegistry, MessageAddedEvent

from .scan import scan_json

__all__ = ["LifestreamCapture", "denied"]

HARNESS = "strands"
MAX_PENDING = 10_000  # events kept while the service is unreachable

Post = Callable[[str, str, bytes], int]  # (url, token, jsonl body) -> HTTP status


def _post(url: str, token: str, body: bytes) -> int:
    req = urllib.request.Request(
        url.rstrip("/") + "/events",
        data=body,
        method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/x-ndjson"},
    )
    try:
        with urllib.request.urlopen(req, timeout=3) as resp:
            return resp.status
    except urllib.error.HTTPError as err:
        return err.code


def denied(patterns: list[str], cwd: str) -> str:
    """The first pattern matching cwd or one of its parents, or ""."""
    if not cwd:
        return ""
    for p in patterns:
        p = p.strip()
        if not p:
            continue
        pat = os.path.normpath(os.path.expanduser(p))
        path = Path(cwd)
        for d in [path, *path.parents]:
            if fnmatch.fnmatchcase(str(d), pat):
                return p
    return ""


def _git(cwd: str) -> dict[str, str] | None:
    def run(*args: str) -> str:
        try:
            out = subprocess.run(["git", "-C", cwd, *args], check=False, capture_output=True, text=True, timeout=2)
        except (OSError, subprocess.SubprocessError):
            return ""
        return out.stdout.strip() if out.returncode == 0 else ""

    commit = run("rev-parse", "HEAD")
    if not commit:
        return None
    remote = run("remote", "get-url", "origin")
    if "://" in remote and "@" in remote.split("://", 1)[1].split("/", 1)[0]:
        scheme, rest = remote.split("://", 1)
        remote = scheme + "://" + rest.split("@", 1)[1]  # drop user:token@
    return {"remote": remote, "branch": run("rev-parse", "--abbrev-ref", "HEAD"), "commit": commit}


def _jsonable(o: Any) -> Any:
    if isinstance(o, bytes | bytearray):
        return base64.b64encode(o).decode("ascii")
    return str(o)


class LifestreamCapture(HookProvider):
    """A Strands hook provider that streams the agent's messages to Lifestream.

    One provider is one session: it gets a fresh session_id and numbers the
    messages it sees from 0. Pass session_id only if it is unique to this
    run, or later messages will collide with earlier ones and be dropped.
    """

    def __init__(
        self,
        session_id: str | None = None,
        *,
        env: Mapping[str, str] | None = None,
        cwd: str | None = None,
        post: Post = _post,
    ) -> None:
        env = os.environ if env is None else env
        self.session_id = session_id or str(uuid.uuid4())
        self.url = env.get("LIFESTREAM_URL", "")
        self.token = env.get("LIFESTREAM_TOKEN", "")
        self.scan = env.get("LIFESTREAM_SCAN", "on") != "off"
        self.cwd = cwd if cwd is not None else os.getcwd()
        deny = denied(env.get("LIFESTREAM_DENY", "").split(":"), self.cwd)
        self.on = env.get("LIFESTREAM_CAPTURE") == "on" and not deny
        if self.on and not (self.url and self.token):
            raise ValueError("LIFESTREAM_URL and LIFESTREAM_TOKEN must be set when LIFESTREAM_CAPTURE=on")
        self.git = _git(self.cwd) if self.on else None
        self.pending: list[bytes] = []
        self.sequence = 0
        self._post = post
        self._warned = False

    def register_hooks(self, registry: HookRegistry, **kwargs: Any) -> None:
        if not self.on:
            return
        registry.add_callback(MessageAddedEvent, self.on_message)
        registry.add_callback(AfterInvocationEvent, lambda _event: self.flush())

    def on_message(self, event: MessageAddedEvent) -> None:
        message, findings = scan_json(event.message) if self.scan else (event.message, {})
        payload: dict[str, Any] = {"session_id": self.session_id, "sequence": self.sequence, "harness": HARNESS}
        model = _model_id(event.agent)
        if model:
            payload["model"] = model
        payload["cwd"] = self.cwd
        if self.git:
            payload["git"] = self.git
        if self.scan:
            payload["scanned"] = True
            if findings:
                payload["findings"] = [{"kind": k, "count": n} for k, n in sorted(findings.items())]
        payload["message"] = message
        ev = {"type": "llm.message", "source": HARNESS, "occurred_at": datetime.now(UTC).isoformat(), "payload": payload}
        self.sequence += 1
        self.pending.append(json.dumps(ev, default=_jsonable).encode() + b"\n")
        del self.pending[:-MAX_PENDING]
        self.flush()

    def flush(self) -> None:
        """Send everything pending; on failure keep it for the next try."""
        if not self.pending:
            return
        try:
            status = self._post(self.url, self.token, b"".join(self.pending))
        except OSError as err:
            status, reason = 0, str(err)
        else:
            reason = f"HTTP {status}"
        if status == 200:
            self.pending.clear()
            self._warned = False
            return
        if 400 <= status < 500:
            # The service refused the batch (bad token, bad event): retrying
            # can't help, and holding it would block every later message.
            print(f"lifestream capture: dropped {len(self.pending)} messages: {reason}", file=sys.stderr)
            self.pending.clear()
            return
        if not self._warned:
            print(f"lifestream capture: will retry: {reason}", file=sys.stderr)
            self._warned = True


def _model_id(agent: Any) -> str:
    try:
        config = agent.model.get_config()
    except (AttributeError, TypeError):  # a model without the usual config
        return ""
    return str(config.get("model_id", "")) if isinstance(config, dict) else ""
