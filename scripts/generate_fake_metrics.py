#!/usr/bin/env python3
"""Generate synthetic Prometheus metrics for testing the Versus AI agent.

This is the metrics analogue of the sample log generator
(`scripts/generate_noisy_logs.py`). Because Prometheus *scrapes* targets
rather than receiving pushes, the standard way to inject synthetic series
from a host script is a **Prometheus Pushgateway**: this script pushes a
realistic, increasing time-series to a pushgateway that Prometheus then
scrapes. The Versus `query_metrics` analyze tool can range-query those
series (e.g. a 5xx rate / latency-quantile rule) while investigating an
incident.

It emits exactly the metric names the `metrics` example's PromQL uses, so
existing rules keep working:

    demo_http_requests_total{service,code}                 (counter)
    demo_http_request_duration_seconds_bucket{service,le}  (histogram)
    demo_http_request_duration_seconds_sum/_count{service} (histogram)

Usage (run from the repo root):

    # Steady, healthy traffic for 60s (low error rate, low latency).
    python3 scripts/generate_fake_metrics.py

    # Point at a different pushgateway / service (reused by the enterprise
    # example, which has a *standing* prometheus source rather than a tool):
    python3 scripts/generate_fake_metrics.py \
        --target http://localhost:9091 --service checkout

Spike mode — mirror the log generator's `--spike` UX. Drive a 5xx + latency
anomaly so the PromQL anomaly rules cross their thresholds:

    # 90s of anomaly: ~45% 500s, p95 latency > 500ms.
    python3 scripts/generate_fake_metrics.py --spike --duration 90

    # Hands-off demo: spike for 60s, then auto-revert to normal for the
    # remainder of the run (the analogue of /spike?seconds=60 then /calm).
    python3 scripts/generate_fake_metrics.py --spike --spike-duration 60 \
        --duration 120

Use --list to print the exact series/labels emitted plus sample PromQL, and
--clear to delete the pushed group from the pushgateway (the cleanup
analogue of /calm).

Optional traces — when --otlp <endpoint> is set (used by the example's
traces overlay), each push also best-effort POSTs one OTLP/HTTP span to a
Tempo backend so `query_traces` has error / latency-outlier traces to read.
This is fully optional and wrapped in try/except: the metrics path never
depends on it.

    python3 scripts/generate_fake_metrics.py --spike \
        --otlp http://localhost:4318 --duration 90

No external packages required — the exposition body is rendered by hand and
pushed with urllib.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import sys
import time
import urllib.error
import urllib.request

# Histogram bucket upper bounds, in seconds. Chosen so normal traffic sits
# in the low buckets (p95 well under 0.5s) and a spike pushes p95 past 0.5s.
BUCKETS = [0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0]

# Metric names — MUST match the PromQL the `metrics` example uses.
COUNTER_NAME = "demo_http_requests_total"
HISTOGRAM_NAME = "demo_http_request_duration_seconds"


class Series:
    """Accumulates cumulative counter + histogram state for one service.

    Counters are cumulative (monotonically increasing) across the whole run
    so Prometheus `rate(...)` works once the pushgateway is scraped twice.
    Histogram buckets are cumulative per Prometheus convention: the count at
    `le=X` includes every observation with duration <= X.
    """

    def __init__(self) -> None:
        # requests[code] -> cumulative count
        self.requests: dict[str, int] = {}
        # cumulative count of observations with duration <= BUCKETS[i]
        self.bucket_counts = [0] * len(BUCKETS)
        self.inf_count = 0
        self.duration_sum = 0.0
        self.duration_count = 0

    def observe(self, code: int, seconds: float) -> None:
        key = str(code)
        self.requests[key] = self.requests.get(key, 0) + 1
        for i, ub in enumerate(BUCKETS):
            if seconds <= ub:
                self.bucket_counts[i] += 1
        self.inf_count += 1
        self.duration_sum += seconds
        self.duration_count += 1


def simulate_tick(series: Series, spiking: bool, rate: int) -> None:
    """Fold ~one interval of synthetic traffic into the cumulative series.

    Normal: steady volume, ~0.5% errors, low latency (rules stay silent).
    Spike:  more volume, ~45% 500s, fat-tailed latency (p95 > 500ms) so the
            anomaly rules cross their thresholds.
    """
    n = random.randint(rate, rate + 10) if spiking else random.randint(rate, rate + 5)
    err_ratio = 0.45 if spiking else 0.005
    for _ in range(n):
        if random.random() < err_ratio:
            code = 500
            latency = random.uniform(0.8, 3.0) if spiking else random.uniform(0.2, 0.6)
        else:
            code = 200
            latency = random.uniform(0.6, 2.0) if spiking else random.uniform(0.02, 0.15)
        series.observe(code, latency)


def _fmt_le(f: float) -> str:
    # Prometheus le labels render whole numbers without a trailing ".0".
    if f == int(f):
        return str(int(f))
    return repr(f)


def render_exposition(series: Series, service: str) -> str:
    """Render the current state in Prometheus text exposition format."""
    lines: list[str] = []
    lines.append("# HELP %s Total demo HTTP requests." % COUNTER_NAME)
    lines.append("# TYPE %s counter" % COUNTER_NAME)
    if not series.requests:
        # Always emit a zero series so the metric exists before the first tick.
        lines.append('%s{service="%s",code="200"} 0' % (COUNTER_NAME, service))
    for code, count in sorted(series.requests.items()):
        lines.append(
            '%s{service="%s",code="%s"} %d' % (COUNTER_NAME, service, code, count)
        )

    lines.append("# HELP %s Request latency in seconds." % HISTOGRAM_NAME)
    lines.append("# TYPE %s histogram" % HISTOGRAM_NAME)
    for i, ub in enumerate(BUCKETS):
        lines.append(
            '%s_bucket{service="%s",le="%s"} %d'
            % (HISTOGRAM_NAME, service, _fmt_le(ub), series.bucket_counts[i])
        )
    lines.append(
        '%s_bucket{service="%s",le="+Inf"} %d'
        % (HISTOGRAM_NAME, service, series.inf_count)
    )
    lines.append(
        '%s_sum{service="%s"} %s' % (HISTOGRAM_NAME, service, repr(series.duration_sum))
    )
    lines.append(
        '%s_count{service="%s"} %d' % (HISTOGRAM_NAME, service, series.duration_count)
    )
    return "\n".join(lines) + "\n"


def pushgateway_url(base: str, job: str) -> str:
    return base.rstrip("/") + "/metrics/job/" + urllib.request.quote(job, safe="")


def push(base: str, job: str, body: str) -> None:
    """PUT the full exposition to the pushgateway group (replaces the group).

    Using PUT (not POST) means each push fully replaces the job's metrics, so
    the cumulative counters we send are exactly what Prometheus scrapes. The
    pushgateway must be scraped with `honor_labels: true` so the `service` /
    `code` / `le` labels in the body are preserved.
    """
    req = urllib.request.Request(
        pushgateway_url(base, job),
        data=body.encode("utf-8"),
        method="PUT",
        headers={"Content-Type": "text/plain; version=0.0.4"},
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        if resp.status >= 300:
            raise RuntimeError("pushgateway returned %d" % resp.status)


def clear(base: str, job: str) -> None:
    """DELETE the pushed group from the pushgateway (cleanup / `/calm`)."""
    req = urllib.request.Request(pushgateway_url(base, job), method="DELETE")
    with urllib.request.urlopen(req, timeout=10) as resp:
        if resp.status >= 300:
            raise RuntimeError("pushgateway DELETE returned %d" % resp.status)


def push_trace(otlp: str, service: str, spiking: bool) -> None:
    """Best-effort: POST one OTLP/HTTP span to Tempo (optional traces path).

    During a spike, ~half the spans are errors with fat latency so
    `query_traces` has anomalies to surface. Wrapped by the caller so a
    missing/slow Tempo never breaks the metrics path.
    """
    is_error = spiking and random.random() < 0.5
    duration_ms = random.uniform(800, 3000) if spiking else random.uniform(20, 150)
    start_ns = time.time_ns()
    end_ns = start_ns + int(duration_ms * 1e6)
    span = {
        "traceId": "%032x" % random.getrandbits(128),
        "spanId": "%016x" % random.getrandbits(64),
        "name": "POST /checkout" if random.random() < 0.5 else "GET /orders",
        "kind": 2,
        "startTimeUnixNano": str(start_ns),
        "endTimeUnixNano": str(end_ns),
        "attributes": [{"key": "http.method", "value": {"stringValue": "POST"}}],
        "status": {"code": 2} if is_error else {"code": 1},
    }
    payload = {
        "resourceSpans": [
            {
                "resource": {
                    "attributes": [
                        {"key": "service.name", "value": {"stringValue": service}}
                    ]
                },
                "scopeSpans": [{"spans": [span]}],
            }
        ]
    }
    req = urllib.request.Request(
        otlp.rstrip("/") + "/v1/traces",
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    urllib.request.urlopen(req, timeout=2).read()


def print_list(service: str) -> None:
    """Print the exact series/labels emitted plus sample PromQL."""
    print("Series emitted (service=%s):" % service)
    print('  %s{service="%s",code="200|500"}' % (COUNTER_NAME, service))
    print(
        '  %s_bucket{service="%s",le="..."}  (+ _sum / _count)'
        % (HISTOGRAM_NAME, service)
    )
    print()
    print("Sample PromQL the metrics example uses:")
    print(
        '  sum by (service) (rate(%s{code=~"5.."}[1m]))   # 5xx rate'
        % COUNTER_NAME
    )
    print(
        "  histogram_quantile(0.95, sum by (le) "
        "(rate(%s_bucket[5m])))   # p95 latency" % HISTOGRAM_NAME
    )


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument(
        "--target", "-t",
        default=os.getenv("PUSHGATEWAY_URL", "http://localhost:9091"),
        help="Prometheus Pushgateway base URL "
             "(default: http://localhost:9091; env: PUSHGATEWAY_URL)",
    )
    ap.add_argument(
        "--service", "-s", default=os.getenv("METRICS_SERVICE", "checkout"),
        help='value for the "service" label on every series '
             "(default: checkout; env: METRICS_SERVICE)",
    )
    ap.add_argument(
        "--job", "-j", default=os.getenv("METRICS_JOB", "demo-traffic"),
        help="pushgateway job grouping label (default: demo-traffic; "
             "env: METRICS_JOB)",
    )
    ap.add_argument(
        "--duration", "-d", type=float, default=60.0,
        help="total seconds to push for; 0 = run until Ctrl+C (default: 60)",
    )
    ap.add_argument(
        "--interval", "-i", type=float, default=2.0,
        help="seconds between pushes (default: 2.0; keep <= Prometheus "
             "scrape_interval so every push is observed)",
    )
    ap.add_argument(
        "--rate", "-r", type=int, default=20,
        help="approximate requests folded in per push (default: 20)",
    )
    ap.add_argument(
        "--spike", action="store_true",
        help="drive a 5xx + latency anomaly (~45%% 500s, p95 > 500ms) so the "
             "PromQL anomaly rules cross their thresholds",
    )
    ap.add_argument(
        "--spike-duration", type=float, default=0.0,
        help="when --spike is set, stay anomalous for only this many seconds "
             "then auto-revert to normal for the rest of the run "
             "(0 = spike for the whole run; default: 0)",
    )
    ap.add_argument(
        "--seed", type=int, default=None,
        help="random seed for reproducible output",
    )
    ap.add_argument(
        "--otlp", default=os.getenv("OTLP_ENDPOINT", ""),
        help="optional Tempo OTLP/HTTP base URL; when set, also POST one "
             "best-effort span per push (env: OTLP_ENDPOINT)",
    )
    ap.add_argument(
        "--list", action="store_true",
        help="print the series/labels emitted and sample PromQL, then exit",
    )
    ap.add_argument(
        "--clear", action="store_true",
        help="delete the pushed group from the pushgateway and exit "
             "(cleanup / the analogue of /calm)",
    )

    args = ap.parse_args()

    if args.list:
        print_list(args.service)
        return 0

    if args.clear:
        try:
            clear(args.target, args.job)
        except (urllib.error.URLError, RuntimeError) as e:
            print("failed to clear pushgateway group: %s" % e, file=sys.stderr)
            return 1
        print("cleared job=%s from %s" % (args.job, args.target))
        return 0

    if args.interval <= 0:
        print("--interval must be > 0", file=sys.stderr)
        return 2
    if args.seed is not None:
        random.seed(args.seed)

    series = Series()
    start = time.time()
    pushes = 0

    print(
        "pushing %s metrics to %s (service=%s, job=%s, %s)"
        % (
            "SPIKING" if args.spike else "normal",
            args.target,
            args.service,
            args.job,
            ("%.0fs" % args.duration) if args.duration > 0 else "until Ctrl+C",
        )
    )

    try:
        while True:
            elapsed = time.time() - start
            if args.duration > 0 and elapsed >= args.duration:
                break

            if args.spike and (args.spike_duration <= 0 or elapsed < args.spike_duration):
                spiking = True
            else:
                spiking = False

            simulate_tick(series, spiking, args.rate)
            try:
                push(args.target, args.job, render_exposition(series, args.service))
            except (urllib.error.URLError, RuntimeError) as e:
                print(
                    "push to %s failed: %s\n"
                    "  Is the pushgateway up? Start the metrics example "
                    "(docker compose up -d) or pass --target." % (args.target, e),
                    file=sys.stderr,
                )
                return 1
            if args.otlp:
                # Best-effort: the metrics path must never break because a
                # trace backend is missing or slow.
                try:
                    push_trace(args.otlp, args.service, spiking)
                except Exception:
                    pass
            pushes += 1
            time.sleep(args.interval)
    except KeyboardInterrupt:
        pass

    total = series.duration_count
    err = series.requests.get("500", 0)
    print(
        "done — %d pushes, %d requests (%d 5xx) over %.0fs to %s"
        % (pushes, total, err, time.time() - start, pushgateway_url(args.target, args.job))
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
