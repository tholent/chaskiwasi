"""strip — the quote-stripping shared service (wasi-server-plan §11.1).

POST /strip: request {text, format_flowed}, response {body, trimmed,
removed_bytes}. Bearer-token authenticated, private network only, no
persistence.

I-1 is a hard requirement here, not a preference: request bodies (letter
text) must never appear in a log line at any level, in this file or in the
WSGI server that runs it. Concretely:

  - This module never calls logging/print with request.data, the parsed
    "text" field, or the response body. The only things logged are the
    request path, status code, and duration — see log_request below.
  - The Dockerfile runs gunicorn WITHOUT --access-logfile. Gunicorn only
    writes an access log if that flag is given; omitting it means there is
    no access log to accidentally leak a body through, full stop, rather
    than trusting a log *format* to exclude the body correctly.

V-11 verifies this by grepping captured container logs for letter
substrings, so "we didn't log it" is a tested property, not an assumption.
"""

from __future__ import annotations

import hmac
import logging
import os
import time

from flask import Flask, Response, g, jsonify, request

from striplib import strip

# Defensive cap, generous relative to the wire's own 500-grapheme /
# 64KB-request limits (wasi-server-plan §4.1, §4.6) — this just keeps a
# malformed or hostile request from forcing a huge talon parse.
MAX_TEXT_BYTES = 256 * 1024

logger = logging.getLogger("strip")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

app = Flask(__name__)


def _bearer_token() -> str | None:
    token = os.environ.get("STRIP_BEARER_TOKEN")
    if not token:
        raise RuntimeError("STRIP_BEARER_TOKEN must be set — refusing to run unauthenticated")
    return token


_TOKEN = _bearer_token()


def _authorized() -> bool:
    auth = request.headers.get("Authorization", "")
    prefix = "Bearer "
    if not auth.startswith(prefix):
        return False
    presented = auth[len(prefix):]
    return hmac.compare_digest(presented, _TOKEN)


@app.before_request
def _require_auth():
    g.start = time.monotonic()
    if request.path == "/healthz":
        return None
    if not _authorized():
        return jsonify(error="unauthorized"), 401
    return None


@app.after_request
def _log_request(response: Response) -> Response:
    # Deliberately: path + status + duration only. Never request.data, never
    # the parsed text, never the response body (see module docstring).
    elapsed_ms = (time.monotonic() - g.get("start", time.monotonic())) * 1000
    logger.info("%s %s -> %s (%.1fms)", request.method, request.path, response.status_code, elapsed_ms)
    return response


@app.get("/healthz")
def healthz():
    return jsonify(status="ok")


@app.post("/strip")
def strip_endpoint():
    content_length = request.content_length or 0
    if content_length > MAX_TEXT_BYTES:
        return jsonify(error="request too large"), 413

    payload = request.get_json(silent=True)
    if not isinstance(payload, dict) or "text" not in payload:
        return jsonify(error="expected JSON body with a 'text' field"), 400

    text = payload["text"]
    if not isinstance(text, str):
        return jsonify(error="'text' must be a string"), 400
    if len(text.encode("utf-8")) > MAX_TEXT_BYTES:
        return jsonify(error="'text' too large"), 413

    format_flowed = bool(payload.get("format_flowed", False))

    result = strip(text, format_flowed)
    return jsonify(
        body=result.body,
        trimmed=result.trimmed,
        removed_bytes=result.removed_bytes,
    )


if __name__ == "__main__":
    # Dev convenience only; the Dockerfile runs gunicorn (see its comment for
    # why: gunicorn's access log is opt-in, so leaving it off entirely is the
    # actual I-1 control, not a config value someone could re-enable later).
    app.run(host="0.0.0.0", port=8080)
