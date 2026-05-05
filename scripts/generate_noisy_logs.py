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

Spike mode — emit a tight burst of one specific template so the agent's
spike detector fires. Typical workflow:

    # 1. Train baseline so the chosen pattern becomes "known".
    python3 scripts/generate_noisy_logs.py --lines 2000

    # 2. Switch the agent to shadow or detect mode and let it catch up.

    # 3. Inject a spike (50 db-conn-refused lines packed into ~10s).
    python3 scripts/generate_noisy_logs.py --append --start-time now \
        --spike db-conn-refused --spike-burst 80

Use --list-templates to see every template name accepted by --spike, or
pass --spike auto to pick one at random.
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


# -----------------------------------------------------------------------------
# Multi-format / multi-language / multi-infrastructure templates
#
# Real production deployments emit logs in many shapes. The agent's
# `service_patterns` regexes are designed to extract a service name from any
# of them, so we mirror a representative sample here. Each function returns
# (level, message); the message body is **already a complete log line** in
# the target format. The driver function still prefixes a timestamp so files
# remain time-ordered.
# -----------------------------------------------------------------------------

# --- Node.js ----------------------------------------------------------------
def t_node_pino_info() -> tuple[str, str]:
    # Pino default JSON line.
    user = random.choice(USERS)
    return "INFO ", (
        f'{{"level":30,"time":{int(random.random()*1e13)},"pid":{random.randint(100,9999)},'
        f'"hostname":"{random.choice(HOSTS)}","service":"checkout-api","msg":"order placed",'
        f'"user":"{user}","duration_ms":{random.randint(20,300)}}}'
    )


def t_node_pino_error() -> tuple[str, str]:
    return "ERROR", (
        f'{{"level":50,"time":{int(random.random()*1e13)},"pid":{random.randint(100,9999)},'
        f'"hostname":"{random.choice(HOSTS)}","service":"checkout-api","msg":"stripe charge failed",'
        f'"err":{{"type":"StripeCardError","code":"card_declined","statusCode":402}}}}'
    )


def t_node_winston() -> tuple[str, str]:
    # Winston JSON.
    return "WARN ", (
        f'{{"level":"warn","timestamp":"2026-05-02T12:34:56.789Z","service":"recommendations",'
        f'"message":"feature flag fallback","flag":"new-ranker","reason":"timeout"}}'
    )


def t_node_express_morgan() -> tuple[str, str]:
    # Combined-log-format from morgan + express, with leading bracketed app name.
    code = random.choice([200, 201, 304, 404, 500])
    return "INFO ", (
        f'[checkout-api] {random_ip()} - - "GET /api/cart HTTP/1.1" {code} '
        f'{random.randint(80, 9000)} "-" "Mozilla/5.0"'
    )


# --- Python -----------------------------------------------------------------
def t_python_json_logger() -> tuple[str, str]:
    # python-json-logger / AWS Lambda Powertools.
    return "ERROR", (
        f'{{"asctime":"2026-05-02 12:34:56,789","name":"orders.api","levelname":"ERROR",'
        f'"service":"orders-worker","message":"sqlalchemy IntegrityError on orders.idx_user_id",'
        f'"trace_id":"{random_trace_id()}"}}'
    )


def t_python_structlog() -> tuple[str, str]:
    # structlog logfmt renderer.
    return "INFO ", (
        f'event="charge captured" service=billing svc=billing-svc '
        f'amount_usd={random.randint(1, 9999)}.{random.randint(0,99):02d} '
        f'gateway=stripe trace_id={random_trace_id()}'
    )


def t_python_loguru() -> tuple[str, str]:
    return "WARN ", (
        f'2026-05-02 12:34:56.789 | WARNING  | orders.tasks:run:42 - '
        f'service=orders-worker celery task retry attempt={random.randint(1,5)} '
        f'task=orders.tasks.send_receipt'
    )


def t_python_django_500() -> tuple[str, str]:
    # Django built-in logger with bracketed app name.
    return "ERROR", (
        f'[django.request] [orders] Internal Server Error: /checkout/  '
        f'Traceback (most recent call last): File "views.py", line 88, '
        f'in checkout — django.db.utils.OperationalError: server closed the connection'
    )


# --- Java / JVM -------------------------------------------------------------
def t_java_logback() -> tuple[str, str]:
    # Spring Boot default pattern: bracketed thread + bracketed logger.
    return "ERROR", (
        f'2026-05-02 12:34:56.789  ERROR 1 --- [http-nio-8080-exec-3] '
        f'[payments-service] c.e.p.PaymentController : '
        f'org.springframework.web.client.HttpServerErrorException$ServiceUnavailable: 503 from upstream'
    )


def t_java_log4j_json() -> tuple[str, str]:
    return "INFO ", (
        f'{{"timestamp":"2026-05-02T12:34:56.789Z","level":"INFO","thread":"kafka-consumer-1",'
        f'"loggerName":"com.example.OrdersConsumer","service":"orders-consumer",'
        f'"message":"committed offset","topic":"orders.created","partition":3,"offset":{random.randint(1_000_000, 9_000_000)}}}'
    )


def t_java_logback_json() -> tuple[str, str]:
    # logstash-logback-encoder / Quarkus structured.
    return "WARN ", (
        f'{{"@timestamp":"2026-05-02T12:34:56.789Z","level":"WARN","thread_name":"vert.x-eventloop-thread-2",'
        f'"logger_name":"io.vertx.core.http","app":"vertx-gateway","message":"connection reset by peer",'
        f'"trace.id":"{random_trace_id()}"}}'
    )


def t_kotlin_micronaut() -> tuple[str, str]:
    return "INFO ", (
        f'2026-05-02 12:34:56.789 [default-nioEventLoopGroup-1-3] INFO  i.m.h.s.netty.NettyHttpServer - '
        f'service=micronaut-edge endpoint=/health status=UP'
    )


# --- Go ---------------------------------------------------------------------
def t_go_zap_json() -> tuple[str, str]:
    return "ERROR", (
        f'{{"level":"error","ts":"2026-05-02T12:34:56.789Z","caller":"server/handler.go:142",'
        f'"msg":"failed to enqueue job","service":"jobrunner","queue":"emails","err":"redis: nil"}}'
    )


def t_go_zerolog() -> tuple[str, str]:
    # zerolog default JSON.
    return "INFO ", (
        f'{{"level":"info","time":"2026-05-02T12:34:56Z","service":"feature-flags",'
        f'"flag":"new-checkout","user":"{random.choice(USERS)}","enabled":true,"message":"flag evaluated"}}'
    )


def t_go_logrus_text() -> tuple[str, str]:
    return "WARN ", (
        f'time="2026-05-02T12:34:56Z" level=warning msg="kafka producer slow" '
        f'service=event-bus svc=event-bus topic=billing.events latency_ms={random.randint(500, 5000)}'
    )


def t_go_slog() -> tuple[str, str]:
    # Go 1.21+ slog default text handler is logfmt-ish.
    return "INFO ", (
        f'time=2026-05-02T12:34:56.789Z level=INFO msg="cache miss" '
        f'service=catalog svc=catalog key=product:{random.randint(1000,9999)}'
    )


# --- Rust -------------------------------------------------------------------
def t_rust_tracing() -> tuple[str, str]:
    return "ERROR", (
        f'2026-05-02T12:34:56.789Z ERROR billing::reconciler: service=billing-reconciler '
        f'mismatch detected account_id={random.randint(100_000_000, 900_000_000)} delta_cents={random.randint(-9999, 9999)}'
    )


# --- .NET / Serilog ---------------------------------------------------------
def t_dotnet_serilog_compact() -> tuple[str, str]:
    return "INFO ", (
        f'{{"@t":"2026-05-02T12:34:56.789Z","@l":"Information","@m":"Order {random.randint(1,9999)} shipped",'
        f'"Service":"orders-shipping","app":"orders-shipping","Carrier":"UPS"}}'
    )


def t_dotnet_serilog_text() -> tuple[str, str]:
    return "WARN ", (
        f'2026-05-02 12:34:56.789 +00:00 [WRN] [billing-api] HTTP retry attempt {random.randint(1,4)} '
        f'for POST /v1/charges (HttpRequestException)'
    )


# --- Ruby -------------------------------------------------------------------
def t_ruby_rails() -> tuple[str, str]:
    method = random.choice(["GET", "POST"])
    return "INFO ", (
        f'I, [2026-05-02T12:34:56.789 #{random.randint(100,9999)}]  INFO -- : '
        f'service=storefront-rails [Rack] {method} /products status=200 '
        f'duration={random.uniform(5, 800):.2f}ms'
    )


def t_ruby_semantic_logger() -> tuple[str, str]:
    return "ERROR", (
        f'2026-05-02 12:34:56.789 E [{random.randint(100,9999)}:worker-3] '
        f'service=storefront-jobs Sidekiq -- Job failed: ChargeJob '
        f'error="ActiveRecord::RecordNotFound: Couldn\'t find Order"'
    )


# --- PHP --------------------------------------------------------------------
def t_php_monolog_json() -> tuple[str, str]:
    return "WARN ", (
        f'{{"message":"deprecated function called","context":{{"fn":"each"}},'
        f'"level":300,"level_name":"WARNING","channel":"app","datetime":"2026-05-02T12:34:56+00:00",'
        f'"extra":{{"service":"legacy-shop"}}}}'
    )


# --- nginx / HAProxy / Envoy ------------------------------------------------
def t_nginx_access_json() -> tuple[str, str]:
    return "INFO ", (
        f'{{"time":"2026-05-02T12:34:56+00:00","remote_addr":"{random_ip()}","service":"nginx-edge",'
        f'"request":"GET /api/health HTTP/1.1","status":{random.choice([200, 200, 200, 404, 502])},'
        f'"body_bytes_sent":{random.randint(20, 4000)},"upstream":"checkout-api:8080"}}'
    )


def t_nginx_error_log() -> tuple[str, str]:
    return "ERROR", (
        f'2026/05/02 12:34:56 [error] {random.randint(1000,9999)}#0: '
        f'*{random.randint(1, 99999)} upstream timed out (110: Connection timed out) '
        f'while reading response header from upstream, client: {random_ip()}, '
        f'server: api.example.com, upstream: "http://10.0.{random.randint(0,255)}.{random.randint(0,255)}:8080/"'
    )


def t_haproxy() -> tuple[str, str]:
    return "INFO ", (
        f'haproxy[{random.randint(100,9999)}]: {random_ip()}:{random.randint(30000,60000)} '
        f'[02/May/2026:12:34:56.789] frontend_https~ backend_checkout/checkout-api-{random.randint(1,5)} '
        f'0/0/0/{random.randint(5, 250)}/{random.randint(5, 250)} '
        f'{random.choice([200, 200, 503])} {random.randint(100, 8000)} - - ---- 1/1/0/0/0 0/0 "GET /api/cart HTTP/1.1"'
    )


def t_envoy_access() -> tuple[str, str]:
    return "INFO ", (
        f'[2026-05-02T12:34:56.789Z] "POST /service/v1/orders HTTP/2" {random.choice([200, 200, 503])} '
        f'- via_upstream - "-" {random.randint(100, 4000)} {random.randint(50, 1500)} '
        f'{random.randint(5, 250)} - "{random_ip()}" "envoy/1.30.0" '
        f'"{random_trace_id()}" "checkout-api.svc.cluster.local" "10.0.0.{random.randint(1,254)}:8080"'
    )


# --- Databases (native log formats) -----------------------------------------
def t_postgres_log() -> tuple[str, str]:
    return "ERROR", (
        f'2026-05-02 12:34:56.789 UTC [{random.randint(100,9999)}] postgres@orders ERROR: '
        f'duplicate key value violates unique constraint "orders_pkey"'
    )


def t_mysql_log() -> tuple[str, str]:
    return "WARN ", (
        f'2026-05-02T12:34:56.789Z {random.randint(1,99)} [Warning] [MY-010055] [Server] '
        f'IP address \'{random_ip()}\' could not be resolved: Name or service not known'
    )


def t_redis_log() -> tuple[str, str]:
    return "WARN ", (
        f'{random.randint(100,9999)}:M 02 May 2026 12:34:56.789 # Background AOF rewrite '
        f'terminated by signal 9'
    )


def t_kafka_broker() -> tuple[str, str]:
    return "ERROR", (
        f'[2026-05-02 12:34:56,789] ERROR [ReplicaManager broker={random.randint(0,4)}] '
        f'Error processing fetch with max size 1048576 from consumer on partition orders-{random.randint(0,11)} '
        f'(kafka.server.ReplicaManager)'
    )


def t_mongodb() -> tuple[str, str]:
    return "WARN ", (
        f'{{"t":{{"$date":"2026-05-02T12:34:56.789Z"}},"s":"W","c":"NETWORK","id":22943,'
        f'"ctx":"conn{random.randint(1,9999)}","msg":"Connection refused","attr":{{"service":"mongo-shard-1"}}}}'
    )


# --- syslog / journald / Docker ---------------------------------------------
def t_syslog_sshd() -> tuple[str, str]:
    return "WARN ", (
        f'sshd[{random.randint(1000,9999)}]: Failed password for root from {random_ip()} '
        f'port {random.randint(30000,60000)} ssh2'
    )


def t_syslog_postfix() -> tuple[str, str]:
    return "INFO ", (
        f'postfix/smtpd[{random.randint(1000,9999)}]: connect from unknown[{random_ip()}]'
    )


def t_syslog_cron() -> tuple[str, str]:
    return "INFO ", (
        f'cron[{random.randint(1000,9999)}]: (root) CMD (/usr/local/bin/backup.sh)'
    )


def t_journald_systemd() -> tuple[str, str]:
    return "INFO ", (
        f'systemd[1]: Started {random.choice(["docker.service", "containerd.service", "kubelet.service"])}.'
    )


def t_docker_json() -> tuple[str, str]:
    # Docker JSON-file driver wraps app stdout — operators usually configure
    # a tag so the line begins with a service-prefix-like header.
    return "INFO ", (
        f'docker[{random.randint(100,9999)}]: container=feature-flags-7b8c9 '
        f'message="evaluated 142 flags" duration_ms={random.randint(5, 200)}'
    )


# --- Kubernetes events ------------------------------------------------------
def t_k8s_kubelet() -> tuple[str, str]:
    return "WARN ", (
        f'kubelet[{random.randint(1000,9999)}]: Failed to pull image "registry.example.com/checkout:v1.2.3": '
        f'rpc error: code = NotFound desc = manifest not known'
    )


def t_k8s_event_json() -> tuple[str, str]:
    return "WARN ", (
        f'{{"kind":"Event","apiVersion":"v1","involvedObject":{{"kind":"Pod","name":"checkout-api-abc",'
        f'"namespace":"prod"}},"reason":"BackOff","message":"Back-off restarting failed container",'
        f'"service.name":"checkout-api","type":"Warning"}}'
    )


# --- Cloud (AWS / GCP / Azure) ---------------------------------------------
def t_aws_lambda() -> tuple[str, str]:
    return "ERROR", (
        f'{{"timestamp":"2026-05-02T12:34:56.789Z","level":"ERROR","service":"order-processor",'
        f'"message":"DynamoDB ProvisionedThroughputExceededException","aws_request_id":"{random_trace_id()}",'
        f'"function_name":"order-processor-prod","function_version":"$LATEST"}}'
    )


def t_aws_cloudwatch_alarm() -> tuple[str, str]:
    return "ERROR", (
        f'{{"AlarmName":"orders-api-5xx","NewStateValue":"ALARM","Region":"{random.choice(REGIONS)}",'
        f'"service":"orders-api","Threshold":5.0,"StateReason":"Threshold crossed: 1 datapoint above 5.0"}}'
    )


def t_aws_alb_access() -> tuple[str, str]:
    return "INFO ", (
        f'http 2026-05-02T12:34:56.789Z app/checkout-alb/abc123 {random_ip()}:{random.randint(30000,60000)} '
        f'10.0.1.{random.randint(1,254)}:8080 0.001 0.045 0.000 '
        f'{random.choice([200, 502, 504])} {random.choice([200, 502, 504])} '
        f'{random.randint(100, 9000)} {random.randint(100, 9000)} '
        f'"GET https://api.example.com:443/api/cart HTTP/2.0" "Mozilla/5.0" '
        f'TLS_AES_128_GCM_SHA256 TLSv1.3 arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/checkout-tg/abc'
    )


def t_aws_eks_cni() -> tuple[str, str]:
    return "WARN ", (
        f'aws-node[{random.randint(100,9999)}]: service=aws-vpc-cni W0502 12:34:56.789 '
        f'IP pool exhausted on node ip-10-0-{random.randint(0,255)}-{random.randint(0,255)}.ec2.internal'
    )


def t_gcp_log_entry() -> tuple[str, str]:
    return "ERROR", (
        f'{{"timestamp":"2026-05-02T12:34:56.789Z","severity":"ERROR","resource":{{"type":"cloud_run_revision",'
        f'"labels":{{"service_name":"orders-api"}}}},"jsonPayload":{{"service.name":"orders-api",'
        f'"message":"context deadline exceeded"}}}}'
    )


def t_gcp_gke() -> tuple[str, str]:
    return "WARN ", (
        f'{{"timestamp":"2026-05-02T12:34:56.789Z","severity":"WARNING","logName":"projects/prod/logs/gke-cluster",'
        f'"jsonPayload":{{"app":"feature-flags","message":"liveness probe failed: HTTP 503"}}}}'
    )


def t_azure_appinsights() -> tuple[str, str]:
    return "ERROR", (
        f'{{"timestamp":"2026-05-02T12:34:56.789Z","level":"Error","cloud_RoleName":"orders-api",'
        f'"service":"orders-api","operation_Id":"{random_trace_id()}","message":"SqlException: Timeout expired"}}'
    )


# --- OpenTelemetry / ECS strict --------------------------------------------
def t_otel_json() -> tuple[str, str]:
    return "ERROR", (
        f'{{"@timestamp":"2026-05-02T12:34:56.789Z","log.level":"error",'
        f'"service.name":"recommendations","service.version":"3.4.1","trace.id":"{random_trace_id()}",'
        f'"span.id":"{uuid.uuid4().hex[:16]}","message":"vector store query failed: OOM on shard 4"}}'
    )


def t_ecs_filebeat() -> tuple[str, str]:
    return "INFO ", (
        f'{{"@timestamp":"2026-05-02T12:34:56.789Z","ecs.version":"8.0.0",'
        f'"service.name":"search-indexer","host.name":"{random.choice(HOSTS)}",'
        f'"event.dataset":"app","message":"reindex completed","docs":{random.randint(1000, 99999)}}}'
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

    # ----- Multi-format / multi-language / multi-infra ----------------------
    # Most of these are "noisy info/warn" so the catalog grows a healthy mix
    # of services. A few errors stay rare so detect/shadow has something to
    # surface across many runtimes.

    # Node.js
    (t_node_pino_info, 8),
    (t_node_pino_error, 1),
    (t_node_winston, 4),
    (t_node_express_morgan, 6),

    # Python
    (t_python_json_logger, 1),
    (t_python_structlog, 6),
    (t_python_loguru, 3),
    (t_python_django_500, 1),

    # Java / Kotlin / JVM
    (t_java_logback, 1),
    (t_java_log4j_json, 6),
    (t_java_logback_json, 4),
    (t_kotlin_micronaut, 4),

    # Go
    (t_go_zap_json, 2),
    (t_go_zerolog, 6),
    (t_go_logrus_text, 4),
    (t_go_slog, 6),

    # Rust / .NET / Ruby / PHP
    (t_rust_tracing, 1),
    (t_dotnet_serilog_compact, 4),
    (t_dotnet_serilog_text, 3),
    (t_ruby_rails, 5),
    (t_ruby_semantic_logger, 1),
    (t_php_monolog_json, 3),

    # Web / proxy / mesh
    (t_nginx_access_json, 8),
    (t_nginx_error_log, 2),
    (t_haproxy, 5),
    (t_envoy_access, 6),

    # Databases / message brokers (native log formats)
    (t_postgres_log, 1),
    (t_mysql_log, 2),
    (t_redis_log, 2),
    (t_kafka_broker, 1),
    (t_mongodb, 2),

    # syslog / journald / Docker
    (t_syslog_sshd, 2),
    (t_syslog_postfix, 4),
    (t_syslog_cron, 4),
    (t_journald_systemd, 3),
    (t_docker_json, 4),

    # Kubernetes
    (t_k8s_kubelet, 2),
    (t_k8s_event_json, 1),

    # Cloud
    (t_aws_lambda, 1),
    (t_aws_cloudwatch_alarm, 1),
    (t_aws_alb_access, 5),
    (t_aws_eks_cni, 2),
    (t_gcp_log_entry, 1),
    (t_gcp_gke, 3),
    (t_azure_appinsights, 1),

    # OpenTelemetry / ECS
    (t_otel_json, 1),
    (t_ecs_filebeat, 4),
]


def weighted_choice(templates):
    fns, weights = zip(*templates)
    return random.choices(fns, weights=weights, k=1)[0]


# Map every template function in TEMPLATES to a short, hyphenated name so it
# can be referenced from the CLI (e.g. --spike db-conn-refused). Built once
# at import time from the function's __name__ minus the leading "t_".
NAMED_TEMPLATES = {fn.__name__.removeprefix("t_").replace("_", "-"): fn
                   for fn, _ in TEMPLATES}


def list_templates() -> None:
    """Print every named template, sorted, for use with --spike."""
    for name in sorted(NAMED_TEMPLATES):
        print(name)


def pick_spike_template(name: str):
    """Resolve a --spike argument to a template function.

    "auto" picks a random template that produces a recognizable, repeatable
    line (i.e. anything in TEMPLATES). An exact name lookup is tried first;
    otherwise the script aborts with a hint.
    """
    if name == "auto":
        return random.choice(list(NAMED_TEMPLATES.values()))
    fn = NAMED_TEMPLATES.get(name)
    if fn is None:
        raise SystemExit(
            f"unknown --spike template: {name!r}. "
            f"Run with --list-templates to see all options."
        )
    return fn


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
    # Spike-mode flags. Use --spike to emit a tight burst of one specific
    # template so the agent's spike detector fires. Typical workflow:
    #   1. Train baseline:   --lines 2000
    #   2. Switch agent to shadow/detect mode
    #   3. Inject spike:     --append --spike db-conn-refused --spike-burst 80
    ap.add_argument("--spike", default=None, metavar="NAME",
                    help='emit a burst of one template (use "auto" to pick at random; '
                         '--list-templates to see all names). When set, --lines is ignored.')
    ap.add_argument("--spike-burst", type=int, default=50,
                    help="number of lines in the spike burst (default: 50)")
    ap.add_argument("--spike-interval-min", type=float, default=0.0,
                    help="minimum seconds between spike lines (default: 0.0)")
    ap.add_argument("--spike-interval-max", type=float, default=0.2,
                    help="maximum seconds between spike lines (default: 0.2)")
    ap.add_argument("--spike-context", type=int, default=0,
                    help="number of regular noisy lines to emit BEFORE the spike (default: 0)")
    ap.add_argument("--list-templates", action="store_true",
                    help="print every template name usable with --spike, then exit")
    args = ap.parse_args()

    if args.list_templates:
        list_templates()
        return 0

    if args.seed is not None:
        random.seed(args.seed)
    if args.interval_max < args.interval_min:
        print("interval-max must be >= interval-min", file=sys.stderr)
        return 2
    if args.spike and args.spike_interval_max < args.spike_interval_min:
        print("spike-interval-max must be >= spike-interval-min", file=sys.stderr)
        return 2

    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    mode = "a" if args.append else "w"

    ts = parse_start_time(args.start_time)
    written = 0
    with out.open(mode, encoding="utf-8") as f:
        if args.spike:
            spike_fn = pick_spike_template(args.spike)
            spike_label = spike_fn.__name__.removeprefix("t_").replace("_", "-")

            # Optional warm-up so the spike doesn't land in a vacuum.
            for _ in range(args.spike_context):
                level, msg = weighted_choice(TEMPLATES)()
                stamp = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
                f.write(f"{stamp} {level} {msg}\n")
                written += 1
                ts += timedelta(seconds=random.uniform(args.interval_min, args.interval_max))

            # The burst itself: same template repeated, tight spacing.
            for _ in range(args.spike_burst):
                level, msg = spike_fn()
                stamp = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
                f.write(f"{stamp} {level} {msg}\n")
                written += 1
                ts += timedelta(seconds=random.uniform(args.spike_interval_min, args.spike_interval_max))

            print(f"spike: {spike_label} × {args.spike_burst} "
                  f"(context={args.spike_context} lines)")
        else:
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
