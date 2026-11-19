import type {
  ChangeItem,
  ChangesAccount,
  ChangesBucket,
  ChangesView,
  CompareView,
} from "@/lib/awsChanges";

// Changes page fixtures, built in code: the usual day (07-changes) and one
// variant per worst-case page (07e-01 … 07e-17). Times sit on one fixed
// "now" so every test reads the same clock.

export const NOW = "2026-10-31T14:10:00.000Z";
const H = 3_600_000;
const M = 60_000;

export function at(offsetMs: number): string {
  return new Date(Date.parse(NOW) + offsetMs).toISOString();
}

/** Today at hh:mm UTC. */
export function utc(hh: number, mm = 0): string {
  return `2026-10-31T${String(hh).padStart(2, "0")}:${String(mm).padStart(2, "0")}:00.000Z`;
}

export const PROD = "204811974821";
export const SHARED = "381904227615";
export const STAGING = "559210038466";

export function accounts(overrides: Partial<Record<string, Partial<ChangesAccount>>> = {}): ChangesAccount[] {
  return [
    { account: PROD, name: "Prod", complete: true, reason: null, as_of: null },
    { account: SHARED, name: "Shared services", complete: true, reason: null, as_of: null },
    { account: STAGING, name: "Staging", complete: true, reason: null, as_of: null },
  ].map((a) => ({ ...a, ...(overrides[a.account] ?? {}) }));
}

export function item(over: Partial<ChangeItem> & { id: string; time: string }): ChangeItem {
  return {
    kind: "change",
    end_time: null,
    account: PROD,
    region: "eu-west-1",
    service: "ecs",
    actor_class: "pipeline",
    actor: "GitHub Actions deploy.yml",
    summary: "Updated checkout-api task definition :41 → :42",
    event_name: "ecs:UpdateService",
    resources: [],
    fields: [],
    error_code: null,
    count: null,
    ...over,
  };
}

export const SUSPECT = item({
  id: "ev-deploy",
  time: utc(13, 58),
  summary: "Updated checkout-api task definition :41 → :42",
  resources: ["arn:aws:ecs:eu-west-1:204811974821:service/prod/checkout-api"],
  fields: [{ field: "taskDefinition", before: "checkout-api:41", after: "checkout-api:42", before_status: "known", missing_permission: null }],
});

export const BURST = item({
  id: "burst-1",
  kind: "burst",
  time: utc(11, 30),
  end_time: utc(11, 42),
  account: SHARED,
  service: "iam",
  actor_class: "pipeline",
  actor: "role reporting-etl",
  summary: "iam:PutRolePolicy on reporting-exports-writer",
  error_code: "AccessDenied",
  count: 214,
});

export const STACK_GROUP = item({
  id: "group-network",
  kind: "group",
  time: utc(9, 10),
  end_time: utc(9, 14),
  service: "cloudformation",
  actor: "terraform-ci",
  summary: "Applied 12 changes to stack network-prod",
  count: 12,
});

export function usualItems(): ChangeItem[] {
  return [
    item({
      id: "ev-asg",
      time: utc(14, 5),
      service: "autoscaling",
      actor_class: "aws_service",
      actor: "AWS Auto Scaling",
      summary: "Auto Scaling set checkout-asg desired capacity 4 → 6",
    }),
    SUSPECT,
    item({
      id: "ev-lambda",
      time: utc(13, 52),
      service: "lambda",
      actor_class: "person",
      actor: "alice@example.com",
      summary: "Set Lambda billing-worker reserved concurrency 50 → 5",
      fields: [{ field: "reservedConcurrency", before: "50", after: "5", before_status: "known", missing_permission: null }],
    }),
    item({
      id: "ev-sg",
      time: utc(12, 31),
      service: "ec2",
      actor_class: "person",
      actor: "bob",
      summary: "Added ingress rule tcp 443 from sg-alb to sg-checkout",
    }),
    BURST,
    STACK_GROUP,
  ];
}

export function buckets(fill: (start: string, i: number) => Partial<ChangesBucket> = () => ({})): ChangesBucket[] {
  const out: ChangesBucket[] = [];
  for (let i = 0; i < 24; i++) {
    const start = at(-23 * H - 10 * M + i * H);
    out.push({ start, pipeline: 0, person: 0, aws_service: 0, unknown: 0, errors: 0, state: "complete", ...fill(start, i) });
  }
  return out;
}

function usualBuckets(): ChangesBucket[] {
  return buckets((start) => {
    const h = new Date(start).getUTCHours();
    if (h === 13) return { pipeline: 2, person: 1 };
    if (h === 11) return { errors: 214 };
    if (h === 12) return { person: 1 };
    if (h === 9) return { pipeline: 12 };
    if (h === 14) return { aws_service: 1 };
    return {};
  });
}

export const ALARM = {
  id: "alarm-1",
  name: "checkout 5xx",
  account: PROD,
  region: "eu-west-1",
  resource_arns: ["arn:aws:ecs:eu-west-1:204811974821:service/prod/checkout-api"],
  start: utc(14, 2),
  start_known: true,
  end: null,
};

/** The usual day: 37 changes, 3 by people, suspect on the 13:58 deploy, everything read. */
export function usualView(over: Partial<ChangesView> = {}): ChangesView {
  return {
    window: { since: at(-24 * H), until: NOW, settled_until: NOW },
    complete: true,
    summary: {
      total: 37,
      total_is_lower_bound: false,
      by_actor_class: { pipeline: 20, person: 3, aws_service: 14, unknown: 0 },
      people: 3,
      routine: 1240,
      errors: 214,
      truncated: false,
    },
    buckets: usualBuckets(),
    alarms: [ALARM],
    alarm_history_starts: null,
    suspect: { status: "found", change_id: SUSPECT.id, alarm_id: ALARM.id, searched: null },
    gaps: [],
    accounts: accounts(),
    regions: [
      { region: "eu-west-1", state: "selected", complete: true },
      { region: "global", state: "global", complete: true },
    ],
    sync: { last_poll_at: NOW, late_arrivals: 0, clock_skew_seconds: 0, first_read_pending: false, dropped: 0 },
    items: usualItems(),
    next_cursor: null,
    ...over,
  };
}

export function usualCompare(over: Partial<CompareView> = {}): CompareView {
  return {
    from: at(-6 * H),
    to: NOW,
    status: "ok",
    history_starts: at(-48 * H),
    complete: true,
    added: [{ resource_arn: "arn:i-1", type: "EC2 instance", name: "i-0c51e9a2", account: PROD, region: "eu-west-1" }],
    removed: [],
    changed: [
      {
        resource_arn: "arn:aws:ecs:eu-west-1:204811974821:service/prod/checkout-api",
        type: "ECS service",
        name: "checkout-api",
        account: PROD,
        region: "eu-west-1",
        fields: [{ field: "taskDefinition", before: "checkout-api:41", after: "checkout-api:42", hidden: false }],
      },
    ],
    partial: [],
    snapshots: { oldest: at(-48 * H), count: 48 },
    changes_between: [],
    changes_between_total: 0,
    ...over,
  };
}

export interface ChangesStateFixture {
  view: ChangesView;
  compare: CompareView;
}

function lower(view: ChangesView, total: number): ChangesView {
  return { ...view, complete: false, summary: { ...view.summary, total, total_is_lower_bound: true } };
}

/** One fixture per worst-case page, keyed by its 07e number. */
export function changesStates(): Record<string, ChangesStateFixture> {
  const usual = usualView();
  const compare = usualCompare();
  return {
    "01-first-sync": {
      view: {
        ...usualView(),
        complete: false,
        summary: { ...usual.summary, total: 8, total_is_lower_bound: true, people: 2 },
        sync: { ...usual.sync, first_read_pending: true },
        gaps: [{ account: PROD, region: "eu-west-1", from: at(-24 * H), to: NOW, reason: "not_checked" }],
      },
      compare,
    },
    "02-cloudtrail-denied": {
      view: {
        ...lower(usual, 31),
        accounts: accounts({ [STAGING]: { complete: false, reason: "cloudtrail_denied" } }),
        gaps: [
          {
            account: STAGING,
            region: "eu-west-1",
            from: at(-24 * H),
            to: NOW,
            reason: "cloudtrail_denied",
            missing_permission: "cloudtrail:LookupEvents",
          },
        ],
      },
      compare,
    },
    "03-history-gap": {
      view: {
        ...lower(usual, 35),
        accounts: accounts({ [PROD]: { complete: false, reason: "throttled" } }),
        gaps: [{ account: PROD, region: "eu-west-1", from: utc(2, 10), to: utc(3, 40), reason: "throttled" }],
        buckets: usualBuckets().map((b) => {
          const h = new Date(b.start).getUTCHours();
          return h === 2 || h === 3 ? { ...b, state: "gap" as const } : b;
        }),
      },
      compare,
    },
    "04-no-changes": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 0, by_actor_class: { pipeline: 0, person: 0, aws_service: 0, unknown: 0 }, people: 0, routine: 0, errors: 0 },
        buckets: buckets(),
        alarms: [],
        suspect: { status: "no_alarm" },
        items: [],
      },
      compare: usualCompare({ added: [], removed: [], changed: [] }),
    },
    "05-aws-service-only": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 14, by_actor_class: { pipeline: 0, person: 0, aws_service: 14, unknown: 0 }, people: 0, errors: 0 },
        alarms: [],
        suspect: { status: "no_alarm" },
        items: usualItems().filter((i) => i.actor_class === "aws_service"),
      },
      compare,
    },
    "06-very-busy": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 5214, people: 41, by_actor_class: { pipeline: 4402, person: 41, aws_service: 771, unknown: 0 } },
        buckets: buckets((start) => (new Date(start).getUTCHours() === 9 ? { pipeline: 1240 } : { pipeline: 120 })),
        next_cursor: "c-page-2",
      },
      compare,
    },
    "07-error-burst": { view: usual, compare },
    "08-alarm-no-suspect": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 35 },
        items: usualItems().filter((i) => i.id !== SUSPECT.id),
        suspect: {
          status: "none",
          alarm_id: ALARM.id,
          searched: { accounts: [PROD], regions: ["eu-west-1", "global"], from: utc(13, 2), to: utc(14, 2) },
        },
      },
      compare,
    },
    "09-compare-unavailable": {
      view: usual,
      compare: usualCompare({
        status: "unavailable",
        from: at(-16 * H),
        history_starts: utc(2, 15),
        complete: false,
        added: [],
        removed: [],
        changed: [],
      }),
    },
    "10-compare-no-diff": { view: usual, compare: usualCompare({ added: [], removed: [], changed: [] }) },
    "11-account-disconnected": {
      view: {
        ...lower(usual, 38),
        accounts: accounts({ [STAGING]: { complete: false, reason: "account_disconnected", as_of: utc(9, 20), disconnected_at: utc(9, 25) } }),
        gaps: [{ account: STAGING, region: "eu-west-1", from: utc(9, 20), to: NOW, reason: "account_disconnected" }],
        items: [
          ...usualItems(),
          item({ id: "ev-staging", time: utc(9, 14), account: STAGING, summary: "Updated staging-api task definition :17 → :18" }),
        ],
      },
      compare,
    },
    "12-single-region": {
      view: {
        ...usual,
        regions: [
          { region: "eu-west-1", state: "selected", complete: true },
          { region: "ap-east-1", state: "not_enabled", complete: false },
          { region: "me-central-1", state: "not_enabled", complete: false },
        ],
        gaps: [
          { account: PROD, region: "ap-east-1", from: at(-24 * H), to: NOW, reason: "region_not_enabled" },
          { account: PROD, region: "me-central-1", from: at(-24 * H), to: NOW, reason: "region_not_enabled" },
        ],
      },
      compare,
    },
    "13-unknown-actor": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 39, by_actor_class: { ...usual.summary.by_actor_class, unknown: 1 } },
        buckets: usualBuckets().map((b) => (new Date(b.start).getUTCHours() === 12 ? { ...b, unknown: 1 } : b)),
        items: [
          ...usualItems(),
          item({
            id: "ev-unknown",
            time: utc(12, 47),
            account: SHARED,
            service: "s3",
            actor_class: "unknown",
            actor: "",
            summary: "Put bucket policy on reporting-exports",
          }),
        ],
      },
      compare,
    },
    "14-beyond-retention": { view: usual, compare },
    "15-late-delivery": {
      view: {
        ...usual,
        window: { since: at(-24 * H), until: NOW, settled_until: at(-15 * M) },
        summary: { ...usual.summary, total: 34 },
        suspect: { status: "no_alarm" },
        alarms: [],
        buckets: usualBuckets().map((b, i, all) => (i === all.length - 1 ? { ...b, state: "arriving" as const } : b)),
      },
      compare,
    },
    "16-hidden-secret": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 38, people: 4 },
        items: [
          ...usualItems(),
          item({
            id: "ev-secret",
            time: utc(13, 40),
            service: "lambda",
            actor_class: "person",
            actor: "alice@example.com",
            summary: "Changed PAYMENTS_API_KEY on Lambda billing-worker",
            fields: [{ field: "environment.PAYMENTS_API_KEY", before: null, after: null, before_status: "hidden", missing_permission: null }],
          }),
        ],
      },
      compare: usualCompare({
        changed: [
          {
            resource_arn: "arn:lambda:billing-worker",
            type: "Lambda function",
            name: "billing-worker",
            account: PROD,
            region: "eu-west-1",
            fields: [{ field: "env PAYMENTS_API_KEY", before: null, after: null, hidden: true }],
          },
        ],
      }),
    },
    "17-multi-account": {
      view: {
        ...lower(usual, 35),
        accounts: accounts({ [STAGING]: { complete: false, reason: "throttled" } }),
        gaps: [{ account: STAGING, region: "eu-west-1", from: utc(2, 10), to: utc(3, 40), reason: "throttled" }],
      },
      compare,
    },
  };
}

/** One fixture per contract amendment the page reads (suspect, gap rate, dropped rows, compare extras, ties). */
export function amendmentStates(): Record<string, ChangesStateFixture> {
  const usual = usualView();
  const compare = usualCompare();
  const tieOlder = item({ id: "ev-deploy-tie", time: SUSPECT.time, actor: "GitHub Actions hotfix.yml", summary: "Updated checkout-api desired count 4 → 6" });
  const between = Array.from({ length: 20 }, (_, i) =>
    item({ id: `between-${i}`, time: at(-(i + 1) * 10 * M), summary: `Updated checkout-api task definition :${60 - i} → :${61 - i}` }),
  );
  return {
    "resource-unknown": {
      view: {
        ...usual,
        summary: { ...usual.summary, total: 35 },
        items: usualItems().filter((i) => i.id !== SUSPECT.id),
        suspect: { status: "resource_unknown", alarm_id: ALARM.id, change_id: null, searched: null },
      },
      compare,
    },
    "too-busy": {
      view: {
        ...lower(usual, 36),
        accounts: accounts({ [PROD]: { complete: false, reason: "too_busy" } }),
        gaps: [{ account: PROD, region: "eu-west-1", from: utc(9, 0), to: utc(10, 0), reason: "too_busy", events_per_hour: 3880 }],
      },
      compare,
    },
    dropped: { view: { ...usual, sync: { ...usual.sync, dropped: 3 } }, compare },
    "compare-unavailable-with-changes": {
      view: usual,
      compare: usualCompare({
        status: "unavailable",
        from: at(-16 * H),
        history_starts: null,
        complete: false,
        added: [],
        removed: [],
        changed: [],
        snapshots: { oldest: utc(2, 15), count: 12 },
        changes_between: between,
        changes_between_total: 57,
      }),
    },
    "suspect-off-page": {
      view: {
        ...usual,
        items: usualItems().filter((i) => i.id !== SUSPECT.id),
        suspect: { status: "found", change_id: SUSPECT.id, alarm_id: ALARM.id, change: SUSPECT, searched: null },
      },
      compare,
    },
    "regions-in-scope": {
      view: {
        ...lower(usual, 36),
        regions: [
          { region: "eu-west-1", state: "selected", complete: true },
          { region: "us-east-1", state: "selected", complete: false },
          { region: "ap-east-1", state: "not_enabled", complete: false },
          { region: "global", state: "global", complete: true },
        ],
        gaps: [{ account: PROD, region: "us-east-1", from: utc(2, 10), to: utc(3, 40), reason: "throttled" }],
      },
      compare,
    },
    "top-actors": {
      view: {
        ...usual,
        summary: {
          ...usual.summary,
          top_actors: [
            { actor: "GitHub Actions deploy.yml", actor_class: "pipeline", count: 3880 },
            { actor: "AWS Auto Scaling", actor_class: "aws_service", count: 14 },
            { actor: "alice@example.com", actor_class: "person", count: 2 },
            { actor: "bob", actor_class: "person", count: 1 },
            { actor: "", actor_class: "unknown", count: 1 },
            { actor: "sixth", actor_class: "person", count: 1 },
          ],
        },
      },
      compare,
    },
    "empty-with-last-change": {
      view: {
        ...usual,
        summary: {
          ...usual.summary,
          total: 0,
          by_actor_class: { pipeline: 0, person: 0, aws_service: 0, unknown: 0 },
          people: 0,
          routine: 0,
          errors: 0,
          last_change_before: at(-72 * H),
        },
        buckets: buckets(),
        alarms: [],
        suspect: { status: "no_alarm" },
        items: [],
      },
      compare,
    },
    "event-name": {
      view: {
        ...usual,
        items: [
          item({
            id: "ev-bucket-policy",
            time: utc(13, 30),
            service: "s3",
            actor_class: "person",
            actor: "bob",
            summary: "Put bucket policy on reporting-exports",
            event_name: "s3:PutBucketPolicy",
            resources: ["arn:aws:s3:::reporting-exports"],
          }),
          ...usualItems(),
        ],
      },
      compare,
    },
    "candidates": {
      view: {
        ...usual,
        items: [usualItems()[0], SUSPECT, tieOlder, ...usualItems().slice(2)],
        suspect: {
          status: "found",
          change_id: SUSPECT.id,
          change: SUSPECT,
          alarm_id: ALARM.id,
          searched: null,
          candidates: [
            { change_id: SUSPECT.id, confidence: 0.8, tied: false },
            { change_id: tieOlder.id, confidence: 0.8, tied: true },
            { change_id: "ev-lambda", confidence: 0.4, tied: false },
          ],
        },
      },
      compare,
    },
    "sync-health": { view: { ...usual, sync: { ...usual.sync, late_arrivals: 12, clock_skew_seconds: -75 } }, compare },
    "compare-history-yesterday": {
      view: usual,
      compare: usualCompare({ status: "unavailable", from: at(-28 * H), history_starts: at(-26 * H), complete: false, added: [], removed: [], changed: [] }),
    },
    "tied-suspects": {
      view: { ...usual, items: [usualItems()[0], SUSPECT, tieOlder, ...usualItems().slice(2)] },
      compare,
    },
  };
}

/** The browser-pass repairs: partly read buckets, day-crossing gaps, the arriving tail, copy and compare look-alikes. */
export function browserPassStates(): Record<string, ChangesStateFixture> {
  const usual = usualView();
  const compare = usualCompare();
  const denied = accounts({ [STAGING]: { complete: false, reason: "throttled" } });
  return {
    "partial-buckets": {
      view: {
        ...lower(usual, 37),
        accounts: denied,
        buckets: usualBuckets().map((b) => ({ ...b, state: "partial" as const, gap_accounts: [STAGING] })),
      },
      compare,
    },
    "gap-whole-day": {
      view: {
        ...lower(usual, 37),
        accounts: accounts({ [PROD]: { complete: false, reason: "throttled" } }),
        gaps: [{ account: PROD, region: "eu-west-1", from: at(-24 * H), to: NOW, reason: "throttled" }],
      },
      compare,
    },
    "arriving-suspect": {
      view: {
        ...usual,
        window: { since: at(-24 * H), until: NOW, settled_until: at(-15 * M) },
        buckets: usualBuckets().map((b, i, all) => (i === all.length - 1 ? { ...b, state: "arriving" as const } : b)),
        suspect: { status: "found", change_id: "ev-asg", alarm_id: ALARM.id, searched: null },
      },
      compare,
    },
    "first-read": {
      view: { ...usual, sync: { ...usual.sync, first_read_pending: true } },
      compare,
    },
    "errors-only": {
      view: {
        ...lower(usual, 0),
        accounts: denied,
        summary: {
          ...usual.summary,
          total: 0,
          total_is_lower_bound: true,
          by_actor_class: { pipeline: 0, person: 0, aws_service: 0, unknown: 0 },
          people: 0,
          routine: 0,
          errors: 36,
        },
        window: { since: at(-24 * H), until: NOW, settled_until: at(-16 * M) },
        gaps: [{ account: STAGING, region: "eu-west-1", from: utc(2, 10), to: utc(3, 40), reason: "throttled" }],
        alarms: [
          { ...ALARM, id: "a-eu", region: "eu-west-1" },
          { ...ALARM, id: "a-us", region: "us-east-1" },
        ],
        alarm_history_starts: utc(1, 0),
        suspect: { status: "incomplete", alarm_id: "a-eu" },
        items: [
          { ...BURST, count: 36 },
          item({
            id: "ev-arn",
            time: utc(12, 0),
            summary: "Service checkout updated",
            fields: [
              {
                field: "task_definition_arn",
                before: null,
                after: "arn:aws:ecs:eu-west-1:204811974821:task-definition/checkout:42",
                before_status: "new_only",
                missing_permission: null,
              },
            ],
          }),
        ],
      },
      compare,
    },
    "quiet-no-alarm": {
      view: { ...usual, alarms: [], suspect: { status: "no_alarm" }, alarm_history_starts: utc(1, 0) },
      compare,
    },
    "compare-partial-reason": {
      view: usual,
      compare: usualCompare({
        complete: false,
        history_starts: at(-4 * H),
        partial: [{ account: STAGING, region: "eu-west-1", service: "ec2", history_starts: at(-4 * H), reason: "throttled" }],
      }),
    },
    "compare-full-with-start": { view: usual, compare: usualCompare({ history_starts: at(-30 * H) }) },
    "compare-look-alikes": {
      view: usual,
      compare: usualCompare({
        complete: true,
        partial: [],
        added: [],
        changed: [],
        removed: [
          { resource_arn: "arn:i-a", type: "instance", name: "versus-web", account: PROD, region: "eu-west-1", id_hint: "i-0a1b" },
          { resource_arn: "arn:i-b", type: "instance", name: "versus-web", account: PROD, region: "eu-west-1", id_hint: "i-9f8e" },
        ],
      }),
    },
  };
}
