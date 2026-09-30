#!/usr/bin/env python3
import argparse
import http.server
import json
import statistics
import threading
import time
import urllib.parse
import urllib.request
import uuid


class Fixture(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b'<!doctype html><title>Read budget fixture</title><main>Ready</main><input aria-label="Amount">'
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--daemon", required=True)
    parser.add_argument("--samples", type=int, default=5)
    args = parser.parse_args()
    if not 1 <= args.samples <= 20:
        parser.error("--samples must be between 1 and 20")
    target = urllib.parse.urlsplit(args.daemon)
    if target.scheme != "http" or target.hostname not in ("127.0.0.1", "localhost", "::1"):
        parser.error("--daemon must be a loopback HTTP endpoint")
    owner = "read-budget-" + uuid.uuid4().hex

    def call(path, body=None, query=None):
        url = args.daemon.rstrip("/") + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(url, data=data, headers={
            "Content-Type": "application/json", "X-Brw-Owner": owner,
        })
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    fixture = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
    thread = threading.Thread(target=fixture.serve_forever, daemon=True)
    thread.start()
    tab = None
    try:
        health = call("/health")
        opened = call("/api/browser/open", {"url": f"http://127.0.0.1:{fixture.server_port}/"})
        tab = str(opened["tab"]["id"])
        rows = []
        for budget in (None, 0, 125):
            samples = []
            for _ in range(args.samples):
                query = {"tab_id": tab, "include": "main"}
                if budget is not None:
                    query["settle_ms"] = budget
                start = time.perf_counter()
                read = call("/api/page/read", query=query)
                samples.append(round((time.perf_counter() - start) * 1000, 2))
                if read.get("main") != "Ready":
                    raise RuntimeError("fixture text changed")
            rows.append({"settle_ms": budget, "samples_ms": samples,
                         "median_ms": statistics.median(samples)})
        snap = call("/api/page/snapshot", query={"tab_id": tab, "role": "textbox"})
        ref = snap["elements"][0]["ref"]
        result = call("/api/page/batch", {"tab_id": tab, "steps": [
            {"action": "fill", "ref": ref, "value": "41"},
            {"action": "assert_value", "ref": ref, "value": "41"},
        ]})
        if not result.get("ok"):
            raise RuntimeError("batch fill alias regression: " + str(result.get("error")))
        print(json.dumps({"version": health.get("version"), "read_budget": rows,
                          "batch_fill_alias": "passed"}, indent=2))
    finally:
        try:
            if tab:
                call("/api/browser/close", {"tab_id": tab})
        finally:
            fixture.shutdown()
            fixture.server_close()
            thread.join()


if __name__ == "__main__":
    main()
