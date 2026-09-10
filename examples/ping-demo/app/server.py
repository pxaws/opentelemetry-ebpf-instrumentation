#!/usr/bin/env python3
"""Ping demo service — one Flask app, role selected by $ROLE.

Runs under gunicorn (see Dockerfile). Flask+gunicorn is the server stack OBI
reliably captures; a raw http.server produced status 499 / 0-byte responses
because OBI missed the response write.

Topology (XP_DRAFT §1):
  checkout  POST /checkout      -> payment POST /charges, inventory POST /reservations
  payment   POST /charges       -> audit   POST /audit
  inventory POST /reservations  (leaf)
  audit     POST /audit         (leaf)

Health profile ($PROFILE, XP_DRAFT §3) shapes this service's own responses:
status-code mix + latency, so OBI's http.server.request.duration shows the
right RED arm (2xx=OK, 4xx=Error, 5xx=Fault). Downstream calls produce
http.client.request.duration (the edge signal).
"""
import os
import random
import socket
import time
import urllib.parse

import cpuwork
import propagate
import requests
import urllib3
from flask import Flask, jsonify

urllib3.disable_warnings()  # self-signed downstream certs in the HTTPS demo

ROLE = os.environ.get("ROLE", "checkout")
PORT = int(os.environ.get("PORT", "8000"))

# Share of each response's target duration spent on real CPU work instead of sleeping. Read at
# IMPORT, not per request: gunicorn runs two workers with no --preload, so module state a request
# mutates reaches one worker and about half the traffic (which is also why POST /fault below is
# per-process, not instance-wide). An environment variable is read by every worker.
#
# Default 0 leaves a host that does not set it behaving exactly as before. The effective value is
# reported from /healthz, because a typo that clamps to zero is otherwise indistinguishable from a
# correct deployment.
CPU_SHARE = cpuwork.resolve_share(os.environ.get("CPU_SHARE", "0"))

# Comma-separated downstream base URLs; picked random.choice so multiple
# instances (payment x3, audit x3) fan out on EC2.
PAYMENT_URLS = [u for u in os.environ.get("PAYMENT_URLS", "").split(",") if u]
INVENTORY_URLS = [u for u in os.environ.get("INVENTORY_URLS", "").split(",") if u]
AUDIT_URLS = [u for u in os.environ.get("AUDIT_URLS", "").split(",") if u]

ROUTES = {
    "checkout": "/checkout",
    "payment": "/charges",
    "inventory": "/reservations",
    "audit": "/audit",
}
SERVE_ROUTE = ROUTES[ROLE]

# XP_DRAFT §3 _PROFILES: status -> (weight, mean_s, min_s, max_s)
PROFILES = {
    "healthy": [(200, 1000, 0.05, 0.005, 0.40)],
    # A permanent, small failure rate: 2% fault and 1% error, and otherwise the healthy latency.
    # `erroring` is a fault INJECTION at roughly half the traffic, which is a thing you arm and then
    # disarm. This is the steady state instead, so a service map always has a non-zero failure leg to
    # read. Both legs are present on purpose: with only a 5xx the Errors tile stays at zero and a
    # reader cannot tell a working query from a broken one.
    "flaky": [
        (200, 970, 0.05, 0.005, 0.40),
        (500, 20, 0.03, 0.002, 0.30),
        (429, 10, 0.01, 0.001, 0.10),
    ],
    "degraded": [(200, 1000, 0.30, 0.05, 2.00)],
    "erroring": [
        (200, 500, 0.06, 0.005, 0.50),
        (404, 120, 0.01, 0.001, 0.08),
        (500, 250, 0.03, 0.002, 0.30),
        (429, 130, 0.01, 0.001, 0.10),
    ],
    "faulty": [
        (200, 200, 0.08, 0.005, 0.60),
        (503, 500, 0.05, 0.002, 1.50),
        (500, 300, 0.04, 0.002, 0.90),
    ],
}

# PROFILE is read per request so it can be flipped at runtime via POST /fault
# (fault injection for the demo) without restarting the process.
_state = {"profile": os.environ.get("PROFILE", "healthy")}


def pick_response():
    rows = PROFILES[_state["profile"]]
    weights = [r[1] for r in rows]
    status, _w, mean, lo, hi = random.choices(rows, weights=weights, k=1)[0]
    latency = min(hi, max(lo, random.triangular(lo, hi, mean)))
    return status, latency


def _targets(base):
    """Expand a base URL into (ip, host_header) per resolved A record, so a
    DNS name with multiple A records (Route53 multivalue) fans out across all
    instances instead of pinning to one pooled connection. Host header keeps
    the logical name (clean server.address); ip is what we connect to."""
    p = urllib.parse.urlparse(base)
    host, port = p.hostname, (p.port or 8000)
    try:
        ips = sorted({ai[4][0] for ai in socket.getaddrinfo(host, port,
                                                             socket.AF_INET, socket.SOCK_STREAM)})
    except socket.gaierror:
        ips = [host]
    hosthdr = f"{host}:{port}" if port else host
    return [(f"{p.scheme}://{ip}:{port}{p.path or ''}", hosthdr) for ip in ips]


def call(base_urls, path):
    """POST to one downstream instance, fanning out across resolved IPs;
    retry another on failure (client-side resilience).

    Returns the downstream's status code, or None when there was nothing to call and when every
    target refused the connection. A transport failure is deliberately NOT reported as a status: it
    produces no downstream response to propagate, and OBI records it on the client metric anyway.
    """
    if not base_urls:
        return None
    targets = []
    for base in base_urls:
        targets += _targets(base.rstrip("/") + path)
    random.shuffle(targets)
    for url, hosthdr in targets:
        try:
            r = requests.post(url, json={}, timeout=5, verify=False, headers={"Host": hosthdr})
            # No retry on a status. Another instance of the same role would answer from the same
            # profile, so retrying would only hide the failure this fleet exists to show.
            return r.status_code
        except requests.RequestException:
            continue
    return None


def fan_out():
    """Call this role's downstreams and return the status the caller should adopt, or None."""
    if ROLE == "checkout":
        return propagate.worst_status(
            [call(PAYMENT_URLS, "/charges"), call(INVENTORY_URLS, "/reservations")]
        )
    if ROLE == "payment":
        return propagate.worst_status([call(AUDIT_URLS, "/audit")])
    return None  # inventory, audit: leaves


app = Flask(__name__)


@app.get("/healthz")
def healthz():
    # cpu_share is the PARSED value, and pid names the worker that answered. Both are needed to
    # check a deployment: with two workers and no preload, twenty samples of one worker look
    # identical to ten of each, so without the pid "sampled until both workers answered" is not
    # something a caller can actually do.
    return jsonify(ok=True, role=ROLE, profile=_state["profile"],
                   cpu_share=CPU_SHARE, pid=os.getpid())


@app.post("/fault")
def fault():
    from flask import request
    p = request.args.get("profile", "degraded")
    if p not in PROFILES:
        return jsonify(error="unknown profile", valid=list(PROFILES)), 400
    _state["profile"] = p
    return jsonify(role=ROLE, profile=p)


def _log_compute(spent):
    """One line per request: the slowdown factor, measured directly.

    Wall time over CPU time IS the factor by which this host is being starved — unquantised, in
    seconds, and independent of the metric path, so a metric artifact cannot be mistaken for a
    correctly sized fault. Without it, sizing the fault means hill-climbing a continuous knob
    against a percentile that only updates every few minutes.

    Only emitted when a share is set. On a host at the default the line would be a behaviour
    change for no gain, and the log driver's own cost lands inside the app's cgroup.
    """
    slowdown = spent.wall_seconds / spent.cpu_seconds if spent.cpu_seconds > 0 else 0.0
    print(f"cpuwork role={ROLE} share={CPU_SHARE:.3f} cpu={spent.cpu_seconds:.4f}s "
          f"wall={spent.wall_seconds:.4f}s slowdown={slowdown:.2f} capped={spent.capped}",
          flush=True)


@app.post(SERVE_ROUTE)
def serve():
    status, latency = pick_response()
    if status < 400:
        # Only orchestrate on the OK path, adopting a dependency's failure as this service's own —
        # see propagate.worst_status for why, and for which status wins.
        downstream = fan_out()
        if downstream is not None:
            status = downstream
    # The split is computed BEFORE the compute runs and the sleep is a fixed proportion of the
    # target, never `latency - elapsed`. Sleeping the remainder would cancel out exactly the
    # contention the compute just absorbed, which is the whole signal.
    split = cpuwork.split_duration(latency, CPU_SHARE)
    if split.cpu_budget_seconds > 0:
        _log_compute(cpuwork.spend_cpu(split.cpu_budget_seconds))
    time.sleep(split.sleep_seconds)
    return jsonify(role=ROLE, status=status), status


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=PORT)
