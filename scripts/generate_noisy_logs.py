#!/usr/bin/env python3
"""Generate a noisy application log file for testing the Versus AI agent.

The output mixes a small number of recurring log templates (so the agent can
cluster them into stable patterns) with a handful of rare ERROR lines (so the
agent has something interesting to surface in shadow/detect mode).

Usage:
    python3 local/scripts/generate_noisy_logs.py            # defaults
    python3 local/scripts/generate_noisy_logs.py --lines 5000 \
        --output local/resource/noisy-app.log

Append mode (useful for live-tail testing while the agent is running):
    python3 local/scripts/generate_noisy_logs.py --append --lines 200 \
        --start-time now
"""

from __future__ import annotations

import argparse
import os
import random
import sys
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

USERS = ["alice@example.com", "bob@example.com", "charlie@example.com", "dana@example.com"]
PATHS_GET = [f"/api/incidents/{i}" for i in range(1000, 9999)]
DB_HOSTS = [f"db-{i:02d}" for i in range(1, 16)]
REDIS_HOSTS = [f"redis-{i:02d}" for i in range(1, 6)]
QUEUES = ["incidents", "notifications", "oncall"]
SERVICES = ["api-gateway", "worker", "scheduler", "notifier", "oncall-router"]
REGIONS = ["us-east-1", "us-west-2", "eu-west-1", "ap-southeast-1"]
HOSTS = [f"host-{i:03d}" for i in range(1, 21)]
PODS = [f"versus-{svc}-{random.randint(0,9)}{random.randint(0,9)}{random.randint(0,9)}" for svc in ["api", "worker", "agent"] for _ in range(3)]
ENDPOINTS_S3 = ["s3://incidents-bucket/raw", "s3://incidents-bucket/archive", "s3://incidents-bucket/exports"]
TLS_HOSTS = ["api.slack.com", "api.pagerduty.com", "graph.microsoft.com", "open.larksuite.com"]


def random_ip() -> str:
    return ".".join(str(random.randint(1, 254)) for _ in range(4))


def random_ipv6() -> str:
    return ":".join(f"{random.randint(0, 0xffff):x}" for _ in range(8))


def random_trace_id() -> str:
    return uuid.uuid4().hex


# Each template returns a single log line (without timestamp/level prefix).
# Weights control how often the template fires; high weights == "noisy" common
# lines, low weights == rare anomalies the agent should learn to flag.
def t_api_post_ok() -> tuple[str, str]:
    user = random.choice(USERS)
    dur = random.randint(20, 250)
    rid = str(uuid.uuid4())
    return "INFO ", (
        f"service=api-gateway method=POST path=/api/incidents status=201 "
        f"duration_ms={dur} user={user} request_id={rid}"
    )


def t_api_get_ok() -> tuple[str, str]:
    user = random.choice(USERS)
    dur = random.randint(20, 250)
    path = random.choice(PATHS_GET)
    return "INFO ", (
        f"service=api-gateway method=GET path={path} status=200 "
        f"duration_ms={dur} user={user}"
    )


def t_rate_limit() -> tuple[str, str]:
    return "WARN ", (
        f'service=api-gateway message="rate limit exceeded for client '
        f'{random_ip()} endpoint /api/incidents"'
    )


def t_worker_processed() -> tuple[str, str]:
    n = random.randint(50, 250)
    return "INFO ", f'service=worker message="processed {n} incidents in last minute"'


def t_worker_lag() -> tuple[str, str]:
    queue = random.choice(QUEUES)
    lag = random.randint(30, 300)
    return "WARN ", (
        f'service=worker queue={queue} lag_seconds={lag} '
        f'message="processing slower than expected"'
    )


def t_db_conn_refused() -> tuple[str, str]:
    host = random.choice(DB_HOSTS)
    return "ERROR", (
        f'service=db-pool message="connection refused to database server '
        f'{host} port 5432"'
    )


def t_redis_timeout() -> tuple[str, str]:
    host = random.choice(REDIS_HOSTS)
    return "ERROR", (
        f'service=cache message="redis command timeout host={host} '
        f'after 5000ms"'
    )


def t_oom_killer() -> tuple[str, str]:
    pid = random.randint(1000, 30000)
    return "ERROR", f"kernel: Out of memory: Killed process {pid} (worker) total-vm:524288kB"


def t_panic() -> tuple[str, str]:
    return "ERROR", 'service=worker message="panic: runtime error: invalid memory address or nil pointer dereference"'


def t_5xx() -> tuple[str, str]:
    user = random.choice(USERS)
    return "ERROR", (
        f'service=api-gateway method=POST path=/api/incidents '
        f'status=503 user={user} message="HTTP/1.1 503 upstream unavailable"'
    )


def t_health_check() -> tuple[str, str]:
    svc = random.choice(SERVICES)
    return "INFO ", f'service={svc} message="health check ok" host={random.choice(HOSTS)}'


def t_auth_login_ok() -> tuple[str, str]:
    user = random.choice(USERS)
    return "INFO ", f'service=auth message="user logged in" user={user} ip={random_ip()}'


def t_auth_login_fail() -> tuple[str, str]:
    user = random.choice(USERS)
    return "WARN ", f'service=auth message="401 unauthorized: invalid credentials" user={user} ip={random_ip()}'


def t_db_query_slow() -> tuple[str, str]:
    host = random.choice(DB_HOSTS)
    dur = random.randint(1500, 9000)
    return "WARN ", (
        f'service=db-pool host={host} duration_ms={dur} '
        f'message="slow query detected on incidents table"'
    )


def t_db_deadlock() -> tuple[str, str]:
    host = random.choice(DB_HOSTS)
    return "ERROR", f'service=db-pool host={host} message="deadlock detected while updating incidents row"'


def t_kafka_publish() -> tuple[str, str]:
    topic = random.choice(["incidents.created", "incidents.acked", "incidents.escalated"])
    partition = random.randint(0, 11)
    offset = random.randint(100000, 999999)
    return "INFO ", f'service=worker message="published event" topic={topic} partition={partition} offset={offset}'


def t_kafka_lag() -> tuple[str, str]:
    topic = random.choice(["incidents.created", "incidents.acked"])
    lag = random.randint(500, 50000)
    return "WARN ", f'service=worker message="consumer lag growing" topic={topic} lag={lag}'


def t_oncall_trigger() -> tuple[str, str]:
    incident = uuid.uuid4()
    return "INFO ", f'service=oncall-router message="oncall triggered" incident_id={incident} provider=pagerduty'


def t_oncall_fail() -> tuple[str, str]:
    return "ERROR", 'service=oncall-router message="failed to trigger oncall: pagerduty API returned 502 Bad Gateway"'


def t_tls_handshake_fail() -> tuple[str, str]:
    host = random.choice(TLS_HOSTS)
    return "ERROR", f'service=notifier message="tls handshake failed: x509 certificate signed by unknown authority" host={host}'


def t_dns_fail() -> tuple[str, str]:
    host = random.choice(TLS_HOSTS)
    return "ERROR", f'service=notifier message="dns lookup failed: no such host" host={host}'


def t_disk_full() -> tuple[str, str]:
    host = random.choice(HOSTS)
    return "ERROR", f'service=storage host={host} message="no space left on device" mount=/var/lib/versus'


def t_high_cpu() -> tuple[str, str]:
    host = random.choice(HOSTS)
    cpu = random.randint(85, 99)
    return "WARN ", f'service=monitor host={host} message="high cpu usage" cpu_percent={cpu}'


def t_gc_pause() -> tuple[str, str]:
    pause = random.randint(800, 5000)
    return "WARN ", f'service=worker message="long gc pause" duration_ms={pause}'


def t_s3_upload_ok() -> tuple[str, str]:
    bucket = random.choice(ENDPOINTS_S3)
    size = random.randint(1024, 50_000_000)
    return "INFO ", f'service=archiver message="uploaded object" target={bucket} bytes={size}'


def t_s3_upload_fail() -> tuple[str, str]:
    bucket = random.choice(ENDPOINTS_S3)
    return "ERROR", f'service=archiver message="s3 upload failed: AccessDenied" target={bucket}'


def t_cron_run() -> tuple[str, str]:
    job = random.choice(["cleanup-old-incidents", "compact-catalog", "rotate-logs", "sync-oncall-roster"])
    dur = random.randint(50, 5000)
    return "INFO ", f'service=scheduler message="cron job finished" job={job} duration_ms={dur}'


def t_cron_fail() -> tuple[str, str]:
    job = random.choice(["cleanup-old-incidents", "rotate-logs"])
    return "ERROR", f'service=scheduler message="cron job failed: exit status 1" job={job}'


def t_trace_request() -> tuple[str, str]:
    return "INFO ", (
        f'service=api-gateway trace_id={random_trace_id()} '
        f'message="incoming request" method={random.choice(["GET","POST","PUT","DELETE"])} '
        f'path=/api/incidents user={random.choice(USERS)}'
    )


def t_4xx_validation() -> tuple[str, str]:
    field = random.choice(["title", "severity", "service", "team_id"])
    return "WARN ", f'service=api-gateway status=400 message="validation failed: missing required field" field={field}'


def t_4xx_notfound() -> tuple[str, str]:
    return "WARN ", f'service=api-gateway status=404 path=/api/incidents/{random.randint(1, 99999)} message="incident not found"'


def t_pod_restart() -> tuple[str, str]:
    pod = random.choice(PODS)
    return "WARN ", f'service=k8s message="pod restarted" pod={pod} reason=CrashLoopBackOff'


def t_circuit_open() -> tuple[str, str]:
    target = random.choice(["pagerduty", "slack", "msteams", "telegram"])
    return "ERROR", f'service=notifier message="circuit breaker opened" target={target} after_failures=5'


def t_metrics_export() -> tuple[str, str]:
    return "INFO ", f'service=metrics message="exported metrics batch" count={random.randint(50, 500)} region={random.choice(REGIONS)}'


# -----------------------------------------------------------------------------
# Production-style anomaly templates
#
# Lines that look like things you might genuinely see (and want to be paged
# about) on a real production cluster: kernel OOM, segfaults, data corruption,
# expired TLS certs, NTP skew, replication lag, lost quorum, unexpected
# shutdowns. They get LOW weights so they remain rare — exactly the kind of
# signal the agent is supposed to surface in shadow / detect mode.
# -----------------------------------------------------------------------------
def t_kernel_oom_distinct() -> tuple[str, str]:
    pid = random.randint(1000, 30000)
    container = random.choice(["versus-worker", "versus-agent", "versus-api"])
    return "ERROR", (
        f'kernel: Out of memory: Killed process {pid} ({container}) '
        f'score 999 anon-rss:2097152kB total-vm:8388608kB'
    )


def t_segfault() -> tuple[str, str]:
    pid = random.randint(1000, 30000)
    return "ERROR", (
        f'kernel: worker[{pid}]: segfault at 7f3c8b21d000 ip 00007f3c8b21d042 '
        f'sp 00007ffc12345678 error 4 in libc.so.6'
    )


def t_data_corruption() -> tuple[str, str]:
    incident = uuid.uuid4()
    return "ERROR", (
        f'service=storage message="checksum mismatch detected: data corruption '
        f'on disk" incident_id={incident} expected=sha256:a1b2c3 got=sha256:deadbeef'
    )


def t_security_breach() -> tuple[str, str]:
    return "ERROR", (
        f'service=auth message="security alert: privilege escalation attempt detected" '
        f'user=root source_ip={random_ip()} target_role=admin action=BLOCKED'
    )


def t_quorum_lost() -> tuple[str, str]:
    cluster = random.choice(["raft-incidents", "raft-oncall", "etcd-main"])
    return "ERROR", (
        f'service=cluster message="quorum lost" cluster={cluster} '
        f'healthy_nodes=1 required=2 leader=unknown'
    )


def t_clock_skew() -> tuple[str, str]:
    host = random.choice(HOSTS)
    skew = random.randint(60, 600)
    return "ERROR", (
        f'service=monitor host={host} message="NTP clock skew exceeds threshold" '
        f'offset_seconds={skew} action=service_paused'
    )


def t_certificate_expired() -> tuple[str, str]:
    host = random.choice(TLS_HOSTS)
    return "ERROR", (
        f'service=notifier message="x509 certificate expired" host={host} '
        f'expired_at=2026-04-29T00:00:00Z chain_position=leaf'
    )


def t_replication_lag() -> tuple[str, str]:
    host = random.choice(DB_HOSTS)
    lag = random.randint(300, 1800)
    return "ERROR", (
        f'service=db-replica replica={host} message="replication lag exceeds RPO" '
        f'lag_seconds={lag} primary=db-primary'
    )


def t_kernel_taint() -> tuple[str, str]:
    host = random.choice(HOSTS)
    return "ERROR", (
        f'kernel: {host} tainted: G W loaded module=nvidia_uvm '
        f'reason="unsigned module loaded into kernel"'
    )


def t_unexpected_shutdown() -> tuple[str, str]:
    pod = random.choice(PODS)
    return "ERROR", (
        f'service=k8s message="unexpected SIGTERM received before grace period" '
        f'pod={pod} signal=15 grace_seconds_remaining=27'
    )


# (template_fn, weight)
TEMPLATES: list[tuple[callable, int]] = [
    # Very common (the noisy baseline the agent should cluster as "boring")
    (t_api_post_ok, 40),
    (t_api_get_ok, 40),
    (t_health_check, 25),
    (t_trace_request, 20),
    (t_metrics_export, 15),
    (t_rate_limit, 15),
    (t_worker_processed, 10),
    (t_kafka_publish, 10),
    (t_auth_login_ok, 8),
    (t_cron_run, 6),
    (t_s3_upload_ok, 6),
    (t_4xx_validation, 5),
    (t_4xx_notfound, 5),
    (t_worker_lag, 5),
    (t_db_query_slow, 4),
    (t_oncall_trigger, 4),
    (t_high_cpu, 4),
    (t_auth_login_fail, 3),
    (t_kafka_lag, 3),
    (t_gc_pause, 3),
    # Rare anomalies (the bits the agent should surface in shadow/detect mode)
    (t_db_conn_refused, 4),
    (t_redis_timeout, 2),
    (t_db_deadlock, 2),
    (t_5xx, 2),
    (t_oncall_fail, 2),
    (t_tls_handshake_fail, 2),
    (t_dns_fail, 2),
    (t_s3_upload_fail, 2),
    (t_pod_restart, 2),
    (t_circuit_open, 2),
    (t_disk_full, 1),
    (t_cron_fail, 1),
    (t_oom_killer, 1),
    (t_panic, 1),
    (t_kernel_oom_distinct, 1),
    (t_segfault, 1),
    (t_data_corruption, 1),
    (t_security_breach, 1),
    (t_quorum_lost, 1),
    (t_clock_skew, 1),
    (t_certificate_expired, 1),
    (t_replication_lag, 1),
    (t_kernel_taint, 1),
    (t_unexpected_shutdown, 1),
]


def weighted_choice(templates):
    fns, weights = zip(*templates)
    return random.choices(fns, weights=weights, k=1)[0]


def parse_start_time(raw: str) -> datetime:
    if raw == "now":
        return datetime.now(timezone.utc)
    # accept RFC3339 / ISO-8601 with trailing Z
    raw = raw.replace("Z", "+00:00")
    return datetime.fromisoformat(raw)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--output", "-o", default="local/resource/noisy-app.log",
                    help="output file path (default: local/resource/noisy-app.log)")
    ap.add_argument("--lines", "-n", type=int, default=2000,
                    help="number of log lines to generate (default: 2000)")
    ap.add_argument("--start-time", default="2026-04-20T12:00:00Z",
                    help='start timestamp (RFC3339, or "now"; default: 2026-04-20T12:00:00Z)')
    ap.add_argument("--interval-min", type=float, default=1.0,
                    help="minimum seconds between lines (default: 1.0)")
    ap.add_argument("--interval-max", type=float, default=5.0,
                    help="maximum seconds between lines (default: 5.0)")
    ap.add_argument("--append", "-a", action="store_true",
                    help="append to output instead of overwriting")
    ap.add_argument("--seed", type=int, default=None,
                    help="random seed for reproducible output")
    args = ap.parse_args()

    if args.seed is not None:
        random.seed(args.seed)
    if args.interval_max < args.interval_min:
        print("interval-max must be >= interval-min", file=sys.stderr)
        return 2

    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    mode = "a" if args.append else "w"

    ts = parse_start_time(args.start_time)
    written = 0
    with out.open(mode, encoding="utf-8") as f:
        for _ in range(args.lines):
            level, msg = weighted_choice(TEMPLATES)()
            stamp = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
            f.write(f"{stamp} {level} {msg}\n")
            written += 1
            ts += timedelta(seconds=random.uniform(args.interval_min, args.interval_max))

    action = "appended to" if args.append else "wrote"
    print(f"{action} {out} ({written} lines, end time {ts.strftime('%Y-%m-%dT%H:%M:%SZ')})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
