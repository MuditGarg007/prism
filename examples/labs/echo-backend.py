#!/usr/bin/env python3
"""A small backend for the caching, security and observability labs.

It listens on 127.0.0.1:9104 and prints every request it receives, so you
can see what Prism sent. The path picks how it answers; see respond().
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    disable_nagle_algorithm = True  # otherwise each answer stalls for about 40 ms

    def respond(self):
        """Returns the status, the headers and the body for this request."""
        path = self.path.split("?")[0]
        if path == "/max-age":
            return 200, {"Cache-Control": "max-age=30"}, "cacheable for 30s\n"
        if path == "/no-store":
            return 200, {"Cache-Control": "no-store"}, "never store this\n"
        if path == "/private":
            return 200, {"Cache-Control": "private, max-age=30"}, "for one user\n"
        if path == "/cookie":
            return 200, {"Cache-Control": "max-age=30", "Set-Cookie": "id=1"}, "has a cookie\n"
        if path == "/etag":
            headers = {"Cache-Control": "max-age=2", "ETag": '"v1"'}
            if self.headers.get("If-None-Match") == '"v1"':
                return 304, headers, ""
            return 200, headers, "a large page\n"
        if path == "/vary":
            lang = self.headers.get("Accept-Language", "en")
            return 200, {"Cache-Control": "max-age=30", "Vary": "Accept-Language"}, f"language: {lang}\n"
        return 200, {}, "hello\n"

    def do_GET(self):
        status, headers, body = self.respond()
        print(f"--> {self.command} {self.path}  answering {status}", flush=True)
        for name, value in self.headers.items():
            print(f"    {name}: {value}", flush=True)
        self.send_response(status)
        for name, value in headers.items():
            self.send_header(name, value)
        if status != 304:
            self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *args):
        pass  # do_GET prints what matters


ThreadingHTTPServer(("127.0.0.1", 9104), Handler).serve_forever()
