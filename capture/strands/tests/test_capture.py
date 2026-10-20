import json
import os
import socket
import subprocess
import time
import urllib.request
from pathlib import Path
from types import SimpleNamespace

import pytest
from strands.hooks import AfterInvocationEvent, HookRegistry, MessageAddedEvent

from lifestream_strands import LifestreamCapture, denied

ON = {"LIFESTREAM_CAPTURE": "on", "LIFESTREAM_URL": "http://lifestream.invalid", "LIFESTREAM_TOKEN": "t"}


class FakeModel:
    def get_config(self):
        return {"model_id": "claude-haiku-4-5-20251001"}


AGENT = SimpleNamespace(model=FakeModel())


class Recorder:
    """A stand-in for the HTTP POST: records bodies, answers what it's told."""

    def __init__(self, *statuses):
        self.statuses = list(statuses)
        self.bodies: list[list[dict]] = []

    def __call__(self, url, token, body):
        self.bodies.append([json.loads(line) for line in body.splitlines()])
        status = self.statuses.pop(0) if self.statuses else 200
        if isinstance(status, Exception):
            raise status
        return status


def wired(provider):
    registry = HookRegistry()
    provider.register_hooks(registry)
    return registry


def say(registry, message):
    registry.invoke_callbacks(MessageAddedEvent(agent=AGENT, message=message))


def test_off_unless_capture_is_on_and_not_denied(tmp_path):
    post = Recorder()
    for env in ({}, {**ON, "LIFESTREAM_CAPTURE": "yes"}, {**ON, "LIFESTREAM_DENY": f"/nowhere:{tmp_path}"}):
        registry = wired(LifestreamCapture(env=env, cwd=str(tmp_path / "sub"), post=post))
        assert not registry.has_callbacks()
    assert post.bodies == []
    with pytest.raises(ValueError):
        LifestreamCapture(env={"LIFESTREAM_CAPTURE": "on"}, cwd=str(tmp_path))


def test_each_message_is_sent_as_it_happens(tmp_path):
    post = Recorder()
    cap = LifestreamCapture("sess-1", env=ON, cwd=str(tmp_path), post=post)
    registry = wired(cap)
    say(registry, {"role": "user", "content": [{"text": "upgrade postgres; key AKIAIOSFODNN7EXAMPLE"}]})
    say(registry, {"role": "assistant", "content": [{"text": "ok"}, {"image": {"format": "png", "source": {"bytes": b"\x89PNG"}}}]})
    assert [len(b) for b in post.bodies] == [1, 1]
    first, second = post.bodies[0][0], post.bodies[1][0]
    assert first["type"] == "llm.message" and first["source"] == "strands"
    p = first["payload"]
    assert (p["session_id"], p["sequence"], p["harness"], p["model"], p["cwd"]) == (
        "sess-1",
        0,
        "strands",
        "claude-haiku-4-5-20251001",
        str(tmp_path),
    )
    assert p["message"]["content"][0]["text"] == "upgrade postgres; key [REDACTED:aws_access_key]"
    assert p["scanned"] is True and p["findings"] == [{"kind": "aws_access_key", "count": 1}]
    assert second["payload"]["sequence"] == 1
    assert second["payload"]["message"]["content"][1]["image"]["source"]["bytes"] == "iVBORw=="


def test_failed_sends_wait_in_memory_for_the_next_message(tmp_path, capsys):
    post = Recorder(OSError("connection refused"), 503, 200)
    cap = LifestreamCapture("sess-2", env=ON, cwd=str(tmp_path), post=post)
    registry = wired(cap)
    say(registry, {"role": "user", "content": [{"text": "one"}]})
    say(registry, {"role": "assistant", "content": [{"text": "two"}]})
    assert len(cap.pending) == 2
    registry.invoke_callbacks(AfterInvocationEvent(agent=AGENT))  # end of the invocation retries
    assert cap.pending == []
    assert [[e["payload"]["sequence"] for e in b] for b in post.bodies] == [[0], [0, 1], [0, 1]]
    assert capsys.readouterr().err.count("will retry") == 1, "warn once per outage"


def test_a_refused_batch_is_dropped_not_retried_forever(tmp_path):
    post = Recorder(401)
    registry = wired(LifestreamCapture("s", env=ON, cwd=str(tmp_path), post=post))
    say(registry, {"role": "user", "content": [{"text": "one"}]})
    say(registry, {"role": "user", "content": [{"text": "two"}]})
    assert [[e["payload"]["sequence"] for e in b] for b in post.bodies] == [[0], [1]]


def test_denied_matches_parents_and_home(tmp_path):
    home = str(Path.home())
    assert denied(["~/work"], f"{home}/work/api") == "~/work"
    assert denied(["/srv/*/secret"], "/srv/a/secret/x") == "/srv/*/secret"
    assert denied(["~/work", ""], f"{home}/play") == ""


BIN = Path(__file__).parents[3] / "bin" / "lifestream"


@pytest.mark.skipif(not BIN.exists(), reason="build ../../bin/lifestream first (mise run build)")
def test_against_the_real_service(tmp_path):
    user = json.loads(subprocess.check_output([BIN, "users", "add", "--data-dir", tmp_path, "--name", "me", "--phone", "+1"]))
    ports = []
    for _ in range(2):
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            ports.append(s.getsockname()[1])
    web, api = (f"127.0.0.1:{p}" for p in ports)
    # mise loads .env into the test's environment; without this the server
    # would dispatch to the real dev agent at LIFESTREAM_AGENT_URL.
    clean = {k: v for k, v in os.environ.items() if not k.startswith(("LIFESTREAM_", "TWILIO_"))}
    go = subprocess.Popen([BIN, "serve", "--data-dir", tmp_path, "--addr", web, "--api-addr", api], env=clean, stderr=subprocess.DEVNULL)
    try:
        for _ in range(100):
            try:
                urllib.request.urlopen(f"http://{api}/healthz", timeout=1)
                break
            except OSError:
                time.sleep(0.05)
        env = {**ON, "LIFESTREAM_URL": f"http://{web}", "LIFESTREAM_TOKEN": user["token"]}
        cap = LifestreamCapture("sess-real", env=env, cwd=str(tmp_path))
        registry = wired(cap)
        for i, role in enumerate(["user", "assistant", "user", "assistant"]):
            say(registry, {"role": role, "content": [{"text": f"message {i}"}]})
        assert cap.pending == []
        # A resend after an unanswered POST: the service drops the repeats.
        cap.sequence = 0
        say(registry, {"role": "user", "content": [{"text": "message 0"}]})
        events = json.load(urllib.request.urlopen(f"http://{api}/api/events?owner={user['id']}"))["events"]
        assert [e["payload"]["sequence"] for e in events] == [0, 1, 2, 3]
        assert all(e["type"] == "llm.message" and e["source"] == "strands" for e in events)
        heads = json.load(urllib.request.urlopen(f"http://{api}/api/events?owner={user['id']}&dispatched=1"))["events"]
        assert len(heads) == 3, "only the session's head goes to the agent"
    finally:
        go.terminate()
        go.wait()
