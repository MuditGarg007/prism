#!/usr/bin/env python3
"""A tiny WebSocket lab: `ws-echo.py server` runs a backend on 127.0.0.1:9105
that accepts the upgrade and sends back whatever it gets, and
`ws-echo.py client URL-PATH` connects through Prism on 127.0.0.1:8080.

It skips WebSocket framing, which is fine here: once a connection is
upgraded, Prism copies bytes without looking at them.
"""
import base64
import hashlib
import socket
import sys
import threading
import time

GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def serve(conn):
    request = conn.recv(4096).decode()
    key = next(line.split(": ")[1] for line in request.split("\r\n") if line.lower().startswith("sec-websocket-key"))
    accept = base64.b64encode(hashlib.sha1((key + GUID).encode()).digest()).decode()
    conn.sendall(
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
        f"Sec-WebSocket-Accept: {accept}\r\n\r\n".encode()
    )
    while data := conn.recv(4096):
        conn.sendall(b"echo: " + data)


def server():
    listener = socket.create_server(("127.0.0.1", 9105))
    while True:
        conn, _ = listener.accept()
        threading.Thread(target=serve, args=(conn,), daemon=True).start()


def client(path):
    conn = socket.create_connection(("127.0.0.1", 8080))
    conn.sendall(
        f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"
        "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n".encode()
    )
    print(conn.recv(4096).decode().split("\r\n")[0])
    for word in ("hello", "world"):
        conn.sendall(word.encode())
        print(conn.recv(4096).decode())
    start = time.time()
    print("now silent, waiting for the connection to close...")
    conn.recv(4096)
    print(f"closed after {time.time() - start:.1f}s of silence")


if sys.argv[1] == "server":
    server()
else:
    client(sys.argv[2])
