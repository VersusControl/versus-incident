// First sync of a newly connected AWS account, one Settings poll after
// another (13-worst-cases fragment 1: services fill in, no fake zeros).
// Typed with the UI's api.ts types; specs import this builder.
import type { AwsAccountSync, AwsServiceSync, AwsSyncState } from "../../../ui/src/lib/api";

export interface FirstSyncPoll {
  description: string;
  sync: AwsAccountSync;
}

export interface FirstSyncFixture {
  description: string;
  account_id: string;
  /** The clock the specs freeze to: 12 s after the last commit. */
  now: string;
  polls: FirstSyncPoll[];
}

const SERVICES = ["ec2", "lambda", "s3", "ecs", "rds", "dynamodb", "sqs", "sns", "elasticache"];

function svc(service: string, state: AwsSyncState, done: number, total: number, gaps: string[] = [], last: string | null = null): AwsServiceSync {
  return { service, state, units_done: done, units_total: total, last_good_at: last, gaps };
}

function poll(description: string, state: AwsSyncState, services: AwsServiceSync[], syncedAt: string | null = null, firstSync = true): FirstSyncPoll {
  const total = services.reduce((s, x) => s + x.units_total, 0);
  return {
    description,
    sync: {
      state,
      synced_at: syncedAt,
      first_sync: firstSync,
      units_done: services.reduce((s, x) => s + x.units_done, 0),
      units_total: total || 18,
      services,
    },
  };
}

const ready = (names: string[], at: string) => names.map((n) => svc(n, "ok", 2, 2, [], at));

export function firstSyncFixture(): FirstSyncFixture {
  const s = SERVICES;
  const p3 = poll(
    "Six ready, ElastiCache denied, SNS throttled",
    "syncing",
    [
      ...ready(s.slice(0, 6), "2026-11-09T10:00:09Z"),
      svc("sqs", "syncing", 1, 2),
      svc("sns", "failed", 1, 2, ["throttled"]),
      svc("elasticache", "failed", 2, 2, ["denied"]),
    ],
    "2026-11-09T10:00:09Z",
  );
  const p4 = structuredClone(p3);
  p4.description = "A later answer briefly lists fewer services";
  p4.sync.services = p4.sync.services.slice(0, 7);
  return {
    description:
      "First sync of a newly connected account, one Settings poll after another (13-worst-cases fragment 1: services fill in, no fake zeros).",
    account_id: "111122223333",
    now: "2026-11-09T10:00:32Z",
    polls: [
      poll("Connected, nothing collected yet", "pending", s.map((n) => svc(n, "pending", 0, 2))),
      poll(
        "Three services ready",
        "syncing",
        [...ready(s.slice(0, 3), "2026-11-09T10:00:05Z"), svc("ecs", "syncing", 1, 2, [], "2026-11-09T10:00:05Z"), ...s.slice(4).map((n) => svc(n, "pending", 0, 2))],
        "2026-11-09T10:00:05Z",
      ),
      p3,
      p4,
      poll(
        "First sync done; SNS and ElastiCache gaps remain",
        "partial",
        [...ready(s.slice(0, 7), "2026-11-09T10:00:20Z"), svc("sns", "failed", 1, 2, ["throttled"]), svc("elasticache", "failed", 2, 2, ["denied"])],
        "2026-11-09T10:00:20Z",
        false,
      ),
    ],
  };
}
