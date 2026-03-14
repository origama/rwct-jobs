from __future__ import annotations

import re
import time
from typing import Any
from urllib.parse import urlparse

from bs4 import BeautifulSoup
from flask import Flask, jsonify, request
from markdownify import markdownify as to_markdown
from scrapling.fetchers import Fetcher

app = Flask(__name__)

DEFAULT_TIMEOUT_MS = 8000
DEFAULT_MAX_CHARS = 2500
DEFAULT_USER_AGENT = "rwct-agent-analyzer/1.0 (+source-page-scraper)"
extract_requests = 0


def _is_http_url(raw_url: str) -> bool:
    try:
        parsed = urlparse((raw_url or "").strip())
    except ValueError:
        return False
    return parsed.scheme in {"http", "https"} and bool(parsed.netloc)


def _normalize_markdown(value: str, max_chars: int) -> str:
    text = (value or "").strip()
    text = text.replace("\r\n", "\n")
    text = re.sub(r"\n{3,}", "\n\n", text)
    text = re.sub(r"[ \t]+\n", "\n", text)
    if max_chars > 0:
        text = text[:max_chars].strip()
    return text


def _raw_html_from_page(page: Any) -> str:
    def _coerce_html(candidate: Any) -> str:
        if isinstance(candidate, bytes):
            try:
                candidate = candidate.decode("utf-8", errors="ignore")
            except Exception:
                return ""
        if isinstance(candidate, str):
            return candidate
        if candidate is None:
            return ""
        try:
            rendered = str(candidate)
        except Exception:
            return ""
        if "<" in rendered and ">" in rendered:
            return rendered
        return ""

    for attr in ("html_content", "html", "content", "raw_html"):
        v = getattr(page, attr, None)
        html = _coerce_html(v)
        if html:
            return html
        if callable(v):
            try:
                candidate = v()  # type: ignore[misc, operator]
                html = _coerce_html(candidate)
                if html:
                    return html
            except Exception:
                continue

    # Best-effort fallback: some implementations stringify to HTML.
    return _coerce_html(page)


def _extract_markdown(page: Any, max_chars: int) -> str:
    raw_html = _raw_html_from_page(page)
    if raw_html:
        focused_html = _focus_main_content(raw_html)
        if focused_html:
            md = to_markdown(focused_html, heading_style="ATX")
            md = _normalize_markdown(md, max_chars)
            if md:
                return md

        md = to_markdown(raw_html, heading_style="ATX")
        md = _normalize_markdown(md, max_chars)
        if md:
            return md

    # Fallback to text-only if HTML source is unavailable.
    text_nodes: list[str] = []
    try:
        text_nodes = page.css("body ::text").getall() or []
    except Exception:
        text_nodes = []
    return _normalize_markdown("\n".join(text_nodes), max_chars)


def _focus_main_content(raw_html: str) -> str:
    try:
        soup = BeautifulSoup(raw_html, "lxml")
    except Exception:
        try:
            soup = BeautifulSoup(raw_html, "html.parser")
        except Exception:
            return ""

    for tag in soup.select(
        "script, style, noscript, svg, path, head, meta, link, iframe, nav, header, footer, aside, form"
    ):
        tag.decompose()

    candidates = []
    main_title = ""
    h1 = soup.find("h1")
    if h1 is not None:
        main_title = " ".join(h1.stripped_strings).strip().lower()
    selectors = [
        "main article",
        "section.listing-container",
        "#job-listing-show-container",
        "#job_show",
        "[data-testid='job-details']",
        "main",
        "article",
        ".listing-container",
    ]
    for selector in selectors:
        try:
            candidates.extend(soup.select(selector))
        except Exception:
            continue

    body = soup.body or soup
    if body is None:
        return ""

    if not candidates:
        candidates = [body]

    best = None
    best_score = -1
    for node in candidates:
        text = " ".join(node.stripped_strings)
        lower = text.lower()
        score = len(text)
        if main_title and main_title in lower:
            score += 6000
        if "about the job" in lower:
            score += 4000
        if "salary" in lower:
            score += 3000
        if "job type" in lower:
            score += 2000
        if "apply now" in lower:
            score += 1000
        if "related jobs" in lower and "about the job" not in lower:
            score -= 5000
        if node.find("h1") is not None:
            score += 2000
        if score > best_score:
            best = node
            best_score = score

    if best is None:
        return ""
    return str(best)


@app.get("/healthz")
def healthz() -> tuple[str, int]:
    return "ok", 200


@app.get("/stats")
def stats() -> tuple[Any, int]:
    return jsonify({"extract_requests": extract_requests}), 200


@app.post("/extract")
def extract() -> tuple[Any, int]:
    global extract_requests
    extract_requests += 1
    payload = request.get_json(silent=True) or {}

    url = str(payload.get("url") or "").strip()
    timeout_ms = int(payload.get("timeout_ms") or DEFAULT_TIMEOUT_MS)
    max_chars = int(payload.get("max_chars") or DEFAULT_MAX_CHARS)
    user_agent = str(payload.get("user_agent") or DEFAULT_USER_AGENT).strip() or DEFAULT_USER_AGENT

    if timeout_ms <= 0:
        timeout_ms = DEFAULT_TIMEOUT_MS
    if max_chars <= 0:
        max_chars = DEFAULT_MAX_CHARS

    if not _is_http_url(url):
        return (
            jsonify(
                {
                    "ok": False,
                    "text": "",
                    "final_url": url,
                    "method": "static",
                    "status_code": 0,
                    "duration_ms": 0,
                    "blocked": False,
                    "error": "invalid url",
                }
            ),
            400,
        )

    started = time.time()
    try:
        page = Fetcher.get(
            url,
            timeout=timeout_ms / 1000,
            headers={"User-Agent": user_agent, "Accept": "text/html,application/xhtml+xml"},
        )
        extracted = _extract_markdown(page, max_chars)
        duration_ms = int((time.time() - started) * 1000)

        if not extracted:
            app.logger.warning("extract empty url=%s duration_ms=%s", url, duration_ms)
            return (
                jsonify(
                    {
                        "ok": False,
                        "text": "",
                        "final_url": url,
                        "method": "static",
                        "status_code": 200,
                        "duration_ms": duration_ms,
                        "blocked": False,
                        "error": "empty extracted text",
                    }
                ),
                200,
            )

        app.logger.info("extract ok url=%s duration_ms=%s chars=%s", url, duration_ms, len(extracted))
        return (
            jsonify(
                {
                    "ok": True,
                    "text": extracted,
                    "final_url": url,
                    "method": "static",
                    "status_code": 200,
                    "duration_ms": duration_ms,
                    "blocked": False,
                    "error": "",
                }
            ),
            200,
        )
    except Exception as exc:  # noqa: BLE001
        duration_ms = int((time.time() - started) * 1000)
        app.logger.warning("extract failed url=%s duration_ms=%s err=%s", url, duration_ms, exc)
        return (
            jsonify(
                {
                    "ok": False,
                    "text": "",
                    "final_url": url,
                    "method": "static",
                    "status_code": 0,
                    "duration_ms": duration_ms,
                    "blocked": False,
                    "error": str(exc),
                }
            ),
            200,
        )


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8088)
