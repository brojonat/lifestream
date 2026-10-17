import json
from pathlib import Path

from lifestream_strands.scan import scan_json, scan_text

CORPUS = json.loads((Path(__file__).parents[3] / "testdata" / "secrets.json").read_text())


def test_every_fake_secret_is_redacted():
    for p in CORPUS["positives"]:
        out, found = scan_text(p["text"])
        assert p["secret"] not in out, f"{p['kind']}: secret survived in {out!r}"
        assert p["kind"] in found, f"{p['kind']}: reported {found}"


def test_ordinary_text_is_unchanged():
    for n in CORPUS["negatives"]:
        assert scan_text(n["text"]) == (n["text"], {}), n["name"]


def test_context_is_kept():
    # The same cases as agent/test/scanner.test.ts and internal/scan.
    cases = {
        "DATABASE_URL=postgres://app:hunter2hunter2@db:5432/x": "DATABASE_URL=postgres://app:[REDACTED:connection_password]@db:5432/x",
        "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345": "Authorization: Bearer [REDACTED:bearer]",
        f"git clone https://ghp_{'a' * 36}@github.com/me/repo": "git clone https://[REDACTED:github_token]@github.com/me/repo",
        f"?key=AIzaSy{'b' * 33}&q=1": "?key=[REDACTED:google_api_key]&q=1",
        "call +15555550100 or (+15555550101)": "call [REDACTED:phone] or ([REDACTED:phone])",
        "build 1+15555550100": "build 1+15555550100",
    }
    for text, want in cases.items():
        assert scan_text(text)[0] == want


def test_json_keeps_keys_and_structure():
    value = {"From": "+15555550100", "Body": "key AKIAIOSFODNN7EXAMPLE", "n": 3, "tags": ["alice@example.com"], "AKIAIOSFODNN7EXAMPLE": []}
    out, found = scan_json(value)
    assert out == {
        "From": "[REDACTED:phone]",
        "Body": "key [REDACTED:aws_access_key]",
        "n": 3,
        "tags": ["[REDACTED:email]"],
        "AKIAIOSFODNN7EXAMPLE": [],
    }
    assert list(out) == list(value)
    assert found == {"phone": 1, "aws_access_key": 1, "email": 1}
