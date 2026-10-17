#!/usr/bin/env python3
"""Bounded, dependency-free TCP baseline over an existing Tailscale connection.

Example:
  python3 fork/transport_baseline.py client --ssh test-host \
      --peer-ip 100.64.0.2 --source-ip 100.64.0.1 --output baseline.json

The SSH connection starts an in-memory, expiring test server. No packages,
files, firewall rules, routes, or Tailscale settings are installed remotely.
This measures short-transfer application goodput, not an iperf saturation test.
"""
import argparse
import concurrent.futures
import datetime
import ipaddress
import json
import pathlib
import secrets
import shlex
import socket
import statistics
import subprocess
import threading
import time

CHUNK = b"x" * 65536
MAX_BYTES = 32 * 1024 * 1024


def receive(sock, size):
    result = bytearray()
    while len(result) < size:
        part = sock.recv(size - len(result))
        if not part:
            raise EOFError("connection closed before all bytes arrived")
        result.extend(part)
    return bytes(result)


def send_header(sock, data):
    data = json.dumps(data).encode()
    sock.sendall(len(data).to_bytes(2, "big") + data)


def consume(sock, size):
    remaining = size
    while remaining:
        part = sock.recv(min(65536, remaining))
        if not part:
            raise EOFError("short transfer")
        remaining -= len(part)


def produce(sock, size):
    remaining = size
    while remaining:
        n = min(len(CHUNK), remaining)
        sock.sendall(CHUNK[:n])
        remaining -= n


def server(args):
    address = ipaddress.ip_address(args.bind)
    if address not in ipaddress.ip_network("100.64.0.0/10"):
        raise ValueError("server must bind a Tailscale IPv4 address")
    stopped = threading.Event()
    deadline = time.monotonic() + 180
    errors = []

    def handle(conn):
        with conn:
            conn.settimeout(12)
            conn.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            try:
                n = int.from_bytes(receive(conn, 2), "big")
                if n > 2048:
                    raise ValueError("oversized header")
                q = json.loads(receive(conn, n))
                if not secrets.compare_digest(q.get("token", ""), args.token):
                    raise ValueError("bad token")
                mode, size = q["mode"], q.get("bytes", 0)
                if not isinstance(size, int) or not 0 <= size <= MAX_BYTES:
                    raise ValueError("transfer size outside bound")
                if mode == "download":
                    produce(conn, size)
                elif mode == "upload":
                    consume(conn, size)
                    conn.sendall(b"OK")
                elif mode == "echo":
                    for _ in range(24):
                        conn.sendall(receive(conn, 128))
                elif mode == "stop":
                    stopped.set()
                    conn.sendall(b"OK")
                else:
                    raise ValueError("unknown mode")
            except (OSError, ValueError, EOFError, KeyError) as e:
                errors.append(str(e))

    with socket.socket() as listener:
        listener.bind((args.bind, 0))
        listener.listen(8)
        listener.settimeout(0.25)
        print(json.dumps({"port": listener.getsockname()[1], "lifetime_seconds": 180}), flush=True)
        while not stopped.is_set() and time.monotonic() < deadline:
            try:
                conn, remote = listener.accept()
            except socket.timeout:
                continue
            if remote[0] != args.source_ip:
                conn.close()
                continue
            threading.Thread(target=handle, args=(conn,), daemon=True).start()
    print(json.dumps({"closed": True, "errors": errors}), flush=True)


def process_snapshot(pid):
    if not pid:
        return None
    result = subprocess.run(["ps", "-p", str(pid), "-o", "time=,rss=,%cpu="],
                            text=True, capture_output=True, check=True)
    fields = result.stdout.split()
    if len(fields) != 3:
        raise RuntimeError("client process unavailable")
    seconds = 0.0
    for component in fields[0].split(":"):
        seconds = seconds * 60 + float(component)
    return {"cpu_seconds": seconds, "rss_kib": int(fields[1]), "ps_cpu_percent": float(fields[2])}


def client(args):
    token = secrets.token_hex(24)
    code = pathlib.Path(__file__).read_text()
    command = shlex.join(["python3", "-u", "-c", code, "server", "--bind", args.peer_ip,
                          "--source-ip", args.source_ip, "--token", token])
    ssh_options = [item for option in args.ssh_option for item in ("-o", option)]
    proc = subprocess.Popen(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
                             *ssh_options, args.ssh, command],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    result = {
        "started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "method": "short fixed-size TCP transfers over the existing installed client; no saturation-speed claim",
        "peer_ip": args.peer_ip,
        "bytes_per_run": 16 * 1024 * 1024,
        "runs": [],
    }
    endpoint = None

    def connect(mode, size=0):
        sock = socket.create_connection(endpoint, timeout=10)
        sock.settimeout(12)
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        send_header(sock, {"token": token, "mode": mode, "bytes": size})
        return sock

    def latency():
        samples = []
        with connect("echo") as sock:
            for _ in range(24):
                start = time.perf_counter()
                sock.sendall(b"p" * 128)
                assert receive(sock, 128) == b"p" * 128
                samples.append((time.perf_counter() - start) * 1000)
                time.sleep(0.03)
        return {"median_ms": statistics.median(samples), "samples_ms": samples}

    try:
        ready = proc.stdout.readline()
        if not ready:
            raise RuntimeError(proc.stderr.read())
        endpoint = (args.peer_ip, json.loads(ready)["port"])
        result["test_port"] = endpoint[1]
        result["idle_before"] = process_snapshot(args.client_pid)
        start = time.monotonic()
        time.sleep(5)
        result["idle_after"] = process_snapshot(args.client_pid)
        result["idle_wall_seconds"] = time.monotonic() - start
        result["latency_before"] = latency()
        for streams in (1, 4):
            for direction in ("download", "upload"):
                for repeat in range(3):
                    size = result["bytes_per_run"] // streams
                    barrier = threading.Barrier(streams + 1)

                    def transfer():
                        # Connect before the common start, but send no payload until it.
                        with socket.create_connection(endpoint, timeout=10) as sock:
                            sock.settimeout(12)
                            barrier.wait(timeout=10)
                            send_header(sock, {"token": token, "mode": direction, "bytes": size})
                            if direction == "download":
                                consume(sock, size)
                            else:
                                produce(sock, size)
                                assert receive(sock, 2) == b"OK"

                    cpu_before = process_snapshot(args.client_pid)
                    with concurrent.futures.ThreadPoolExecutor(max_workers=streams) as pool:
                        futures = [pool.submit(transfer) for _ in range(streams)]
                        start = time.perf_counter()
                        barrier.wait(timeout=10)
                        start = time.perf_counter()
                        for future in futures:
                            future.result(timeout=15)
                        seconds = time.perf_counter() - start
                    run = {"streams": streams, "direction": direction, "repeat": repeat + 1,
                           "bytes": result["bytes_per_run"], "seconds": seconds,
                           "mbit_s": result["bytes_per_run"] * 8 / seconds / 1e6,
                           "client_process_before": cpu_before,
                           "client_process_after": process_snapshot(args.client_pid)}
                    result["runs"].append(run)
                    print(json.dumps(run), flush=True)
                    time.sleep(0.4)
        result["latency_after"] = latency()
    finally:
        if endpoint:
            try:
                with connect("stop") as sock:
                    assert receive(sock, 2) == b"OK"
                out, err = proc.communicate(timeout=15)
                result["server_cleanup"] = {"returncode": proc.returncode, "output": out, "stderr": err}
                try:
                    with socket.create_connection(endpoint, timeout=2):
                        result["test_port_closed"] = False
                except OSError:
                    result["test_port_closed"] = True
            except Exception as e:
                result["cleanup_error"] = str(e)
                proc.terminate()
        elif proc.poll() is None:
            proc.terminate()
        result["completed_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        pathlib.Path(args.output).write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="mode", required=True)
    s = sub.add_parser("server")
    s.add_argument("--bind", required=True)
    s.add_argument("--source-ip", required=True)
    s.add_argument("--token", required=True)
    c = sub.add_parser("client")
    c.add_argument("--ssh", required=True)
    c.add_argument("--ssh-option", action="append", default=[],
                   help="additional OpenSSH option; repeat for HostName/HostKeyAlias overrides")
    c.add_argument("--peer-ip", required=True)
    c.add_argument("--source-ip", required=True)
    c.add_argument("--client-pid", type=int)
    c.add_argument("--output", required=True)
    args = parser.parse_args()
    (server if args.mode == "server" else client)(args)
