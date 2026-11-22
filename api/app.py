"""API liviana sobre SearXNG: límite de resultados + caché + CORS.

Solo stdlib, sin dependencias. Pensada para VPS chicos.
"""
import hashlib
import json
import os
import sqlite3
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SEARX_URL = os.environ.get("SEARX_URL", "http://searxng:8080").rstrip("/")
PORT = int(os.environ.get("PORT", "8080"))
DEFAULT_LIMIT = int(os.environ.get("DEFAULT_LIMIT", "20"))
MAX_LIMIT = int(os.environ.get("MAX_LIMIT", "50"))
CACHE_TTL = int(os.environ.get("CACHE_TTL", "3600"))
CACHE_DB = os.environ.get("CACHE_DB", "/data/cache.db")
API_KEY = os.environ.get("API_KEY", "").strip()
UPSTREAM_TIMEOUT = float(os.environ.get("UPSTREAM_TIMEOUT", "25"))
STATIC_DIR = os.environ.get("STATIC_DIR", "/app/public")

_lock = threading.Lock()
_stats = {"requests": 0, "cache_hits": 0, "errors": 0}


def db():
    conn = sqlite3.connect(CACHE_DB, timeout=10)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    return conn


def init_db():
    os.makedirs(os.path.dirname(CACHE_DB) or ".", exist_ok=True)
    with db() as conn:
        conn.execute(
            "CREATE TABLE IF NOT EXISTS cache ("
            "k TEXT PRIMARY KEY, body TEXT NOT NULL, created REAL NOT NULL)"
        )
        conn.execute("CREATE INDEX IF NOT EXISTS idx_created ON cache(created)")


def cache_get(key):
    with db() as conn:
        row = conn.execute(
            "SELECT body FROM cache WHERE k = ? AND created > ?",
            (key, time.time() - CACHE_TTL),
        ).fetchone()
    return json.loads(row[0]) if row else None


def cache_put(key, payload):
    body = json.dumps(payload, ensure_ascii=False)
    with db() as conn:
        conn.execute(
            "INSERT OR REPLACE INTO cache (k, body, created) VALUES (?, ?, ?)",
            (key, body, time.time()),
        )
        # poda barata: no deja crecer el archivo sin control
        conn.execute("DELETE FROM cache WHERE created < ?", (time.time() - CACHE_TTL * 2,))


def search(params, limit):
    qs = {k: v for k, v in params.items() if v}
    qs["format"] = "json"
    url = SEARX_URL + "/search?" + urllib.parse.urlencode(qs, doseq=True)
    req = urllib.request.Request(
        url, headers={"User-Agent": "searxng-api/1.0", "Accept": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=UPSTREAM_TIMEOUT) as resp:
        data = json.loads(resp.read())
    data["results"] = data.get("results", [])[:limit]
    return data


class Handler(BaseHTTPRequestHandler):
    server_version = "searxng-api/1.0"

    def log_message(self, fmt, *args):
        pass

    def _send(self, code, body, ctype="application/json; charset=utf-8", extra=None):
        raw = body if isinstance(body, bytes) else body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Headers", "X-API-Key, Content-Type")
        self.send_header("Access-Control-Allow-Methods", "GET, OPTIONS")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(raw)

    def _json(self, code, obj, extra=None):
        self._send(code, json.dumps(obj, ensure_ascii=False), extra=extra)

    def do_OPTIONS(self):
        self._send(204, b"")

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        params = {k: v[0] for k, v in urllib.parse.parse_qs(parsed.query).items()}

        if path == "/health":
            return self._json(200, {"status": "ok", "cache_ttl": CACHE_TTL})

        if path == "/stats":
            return self._json(200, dict(_stats))

        if path == "/":
            return self._serve_static("index.html")

        if path == "/search":
            return self._handle_search(params)

        return self._json(404, {"error": "ruta no encontrada", "paths": ["/search", "/health", "/stats"]})

    def _serve_static(self, name):
        full = os.path.join(STATIC_DIR, name)
        if not os.path.isfile(full):
            return self._json(404, {"error": "archivo no encontrado"})
        ctype = "text/html; charset=utf-8" if name.endswith(".html") else "text/plain; charset=utf-8"
        with open(full, "rb") as fh:
            self._send(200, fh.read(), ctype=ctype)

    def _handle_search(self, params):
        if API_KEY:
            key = params.get("key") or self.headers.get("X-API-Key", "")
            if key != API_KEY:
                return self._json(401, {"error": "api key invalida o ausente"})

        q = (params.get("q") or "").strip()
        if not q:
            return self._json(400, {"error": "falta el parametro q"})

        try:
            limit = int(params.get("limit", DEFAULT_LIMIT))
        except ValueError:
            return self._json(400, {"error": "limit debe ser un numero"})
        limit = max(1, min(limit, MAX_LIMIT))

        allowed = {"lang", "category", "engines", "time_range", "pageno", "safesearch", "language"}
        upstream = {"q": q, "language": params.get("lang") or params.get("language") or ""}
        for k in allowed:
            if params.get(k):
                upstream[k] = params[k]
        upstream.setdefault("language", "es")

        cache_key = hashlib.sha256(
            json.dumps([upstream, limit], sort_keys=True).encode()
        ).hexdigest()

        with _lock:
            _stats["requests"] += 1

        started = time.time()
        cached = cache_get(cache_key)
        if cached is not None:
            with _lock:
                _stats["cache_hits"] += 1
            cached["cached"] = True
            cached["took_ms"] = int((time.time() - started) * 1000)
            return self._json(200, cached, extra={"X-Cache": "HIT"})

        try:
            data = search(upstream, limit)
        except urllib.error.HTTPError as exc:
            with _lock:
                _stats["errors"] += 1
            return self._json(502, {"error": f"searxng respondio {exc.code}"})
        except Exception as exc:
            with _lock:
                _stats["errors"] += 1
            return self._json(502, {"error": f"fallo la consulta: {exc}"})

        payload = {
            "query": data.get("query", q),
            "count": len(data["results"]),
            "limit": limit,
            "results": data["results"],
            "suggestions": data.get("suggestions", []),
            "unresponsive_engines": data.get("unresponsive_engines", []),
            "cached": False,
            "took_ms": int((time.time() - started) * 1000),
        }

        if payload["results"]:
            cache_put(cache_key, payload)

        return self._json(200, payload, extra={"X-Cache": "MISS"})


def main():
    init_db()
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"api escuchando en :{PORT} -> {SEARX_URL}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
