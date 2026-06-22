# Project Governance

Versus Incident uses a lightweight **benevolent maintainer** governance
model. This document captures how decisions get made and how
contributors can grow into maintainer roles. It is intentionally brief
and will evolve as the project grows.

## Roles

### Users

Anyone running Versus Incident or reading the documentation. Users
participate by filing issues, asking questions in GitHub Discussions, and
sharing their experience.

### Contributors

Anyone whose pull request, documentation change, or issue triage has been
merged or accepted. Contributors are listed automatically by GitHub on the
[contributors page](https://github.com/VersusControl/versus-incident/graphs/contributors).

### Maintainers

Maintainers have write access to the repository and are responsible for:

- Reviewing and merging pull requests.
- Triaging issues and applying labels.
- Cutting releases.
- Enforcing the [Code of Conduct](CODE_OF_CONDUCT.md).
- Stewarding the [roadmap](ROADMAP.md).

The current maintainers are listed in [`MAINTAINERS.md`](MAINTAINERS.md)
when that file exists; otherwise the maintainer team is the set of users
with write access on the GitHub repo.

### Lead maintainer

The **lead maintainer** is the project's founder and final decision-maker.
The lead maintainer:

- Has the final say in case of irreconcilable disagreement among
  maintainers.
- Owns the project's vision and long-term direction.
- Is responsible for handling security reports of last resort.
- Manages the GitHub organization, package registries, and DNS.

The current lead maintainer is **@hoalongnatsu**.

## How decisions are made

The project follows **lazy consensus**:

1. Anyone can propose a change by opening a pull request, an issue, or a
   discussion.
2. Maintainers review. If no maintainer objects within a reasonable time
   (usually 3–7 days for non-trivial changes), the change is approved.
3. Objections are resolved through discussion on the PR / issue. If
   discussion does not converge, the lead maintainer decides.

For changes that affect users in a breaking way (config file format,
HTTP API, default behavior, license), maintainers will:

- Require an explicit issue or discussion thread first.
- Tag at least one other maintainer for review.
- Document the change in release notes.

## Becoming a maintainer

Contributors are invited to become maintainers based on **sustained,
high-quality contribution** — typically:

- Multiple non-trivial merged PRs over a period of months.
- Consistent help with issue triage or code review.
- Demonstrated alignment with the project's direction and Code of Conduct.

Existing maintainers nominate new maintainers; the lead maintainer
confirms. There is no fixed quota.

## Stepping down

Maintainers may step down at any time by opening a PR removing themselves
from `MAINTAINERS.md` (or notifying the lead maintainer). Maintainers who
have been inactive for 12 months may be moved to an "emeritus" list to
keep write access surface tight.

## Funding & sponsorship

Sponsorship policy and tiers are documented in [`SPONSORS.md`](SPONSORS.md).
Sponsorship does **not** purchase merge decisions, roadmap commitments
beyond what is publicly documented, or closed-source carve-outs.

Funds received via GitHub Sponsors and Open Collective are used for:

- Maintainer time.
- Hosting and infrastructure (CI minutes, demo environment, docs site).
- Security audits and signing infrastructure.
- Contributor stipends, travel for conference talks, and program-related
  expenses.

## Open-core commitment

Versus is developed as an **open-core** project. Versus Incident — this
repository — is the **MIT-licensed core** and is free forever. A separate
commercial tier — **Versus Enterprise** (self-hosted commercial license) —
exists for organization-scaling capabilities. (A hosted **Versus Cloud** built
on the same binary is a possible *future* offering; it is **not sold today**, and
the architecture is deliberately kept Cloud-ready for it.) This section is the
**binding promise** about where the line between OSS and the commercial tier
sits. It is what contributors, users, and buyers rely on; maintainers must honour
it on every change.

> **Short version: Open Core, not Open Bait.** Nothing that is in the OSS core
> today will ever be moved out of it to sell it back to you. Only *new or unexpected issues*
> features can be Enterprise-only.

### 1. The one-way ratchet — nothing migrates out of OSS

- A capability that ships under MIT in `versus-incident` **stays under MIT,
  forever.** It is never removed, paywalled, license-gated, or relicensed to
  push users toward the paid tier.
- The Enterprise tier may only ever contain **new** capabilities that did not
  previously exist in the OSS core. "New" means new behaviour, not a re-packaged
  or re-named version of something the core already did.
- This is a **one-way ratchet**: the set of OSS features only grows. If a
  feature is ever in doubt, it stays in OSS.

### 2. The OSS-vs-Enterprise boundary

Use this test for every proposed feature:

1. **Does single-tenant, self-hosted Versus still work in production without
   it?** If removing it would cripple a single-tenant operator's use of the
   product, it belongs in **OSS**.
2. **Does it primarily exist to scale an _organization_** — multi-tenancy, SSO,
   SCIM, RBAC, audit trails, per-org model/billing isolation, managed hosting,
   air-gapped supply-chain packaging, marketplace procurement? Then it **may** be
   **Enterprise**.
3. A **generic, reusable seam** (an interface, hook, or extension point) belongs
   in **OSS**. The **org-scaling logic that consumes that seam** belongs in
   **Enterprise**. We add the seam to OSS first, then build the enterprise
   wrapper against it — never enterprise-specific code in the public tree.

In practice:

| Stays OSS (MIT, single-tenant-useful) | May be Enterprise (org-scaling wrapper) |
|---|---|
| AI detect/analyze agents, drain miner, EWMA, redaction | Multi-tenant org isolation |
| All channels (Slack, Telegram, Teams, Viber, Email, Lark) | SSO (OIDC, SAML), SCIM provisioning |
| All signal sources (Elasticsearch, Graylog, Splunk, file, …) | RBAC + audit log + audit UI |
| On-call integrations (PagerDuty, Opsgenie, AWS IM) | Per-org model gateway / BYO-key isolation |
| Storage providers incl. the Postgres backend | Signed-image / SBOM / air-gapped bundle |
| Incident UI, pattern/incident/analysis management APIs | Managed hosting, marketplace billing & procurement |
| The generic extension hooks the enterprise tier consumes | HA / multi-instance clustering |

> The Postgres storage backend ships **in OSS** precisely so single-tenant
> users benefit. Only the *multi-tenant scoping* layered on top of it is
> enterprise-gated. That is the boundary in action: the seam is open, the
> org-scaling wrapper is paid.

> **"All signal sources" means the log-based sources shipped under MIT**
> (Elasticsearch, Graylog, Splunk, Loki, CloudWatch Logs, file, …) — these are
> OSS forever under §1. A **standing metric/trace ingestion source** (a source
> that polls a PromQL/TraceQL rule each tick to *start* incidents) has **not**
> shipped under MIT and is a **new** Enterprise capability; only the on-demand
> `query_metrics`/`query_traces` correlation tools, the shared queriers, and the
> generic `core.SignalSource` seam + registration hook are OSS. This is the
> seam-in-OSS / wrapper-in-Enterprise pattern, not a narrowing of the OSS surface
> (nothing MIT-released moves out).

#### Documentation is part of the open core

The product **documentation content** (the Markdown under `versus-incident/src`)
is part of the MIT-licensed open core and **stays in this public repository,
forkable and open to pull requests, forever.** Removing the docs *source* from
the public repo — even if the rendered site stays publicly reachable — is a
one-way narrowing of the OSS surface and is **out of bounds** under §1 and §5.

A separate, private **docs application / theme / renderer** (e.g. a Warp-style
docs shell) **may** live in `versus-management-platform`; it acts purely as a
build tool that **pulls the public `versus-incident/src` Markdown at build time**
(git submodule, sparse checkout, or a content fetch step) and emits the static
site deployed to the public docs domain. The renderer may be private; the
**content it renders must remain in the public MIT repo.**

**Enterprise features are documented (not licensed) in the open.** Enterprise-tier
capabilities are documented openly on the public docs site — including how to run
them — for transparency and to show tier value; documenting an Enterprise feature
publicly does **not** make it OSS, and such pages must clearly label the feature
as Enterprise and state that running it requires a licensed Enterprise
distribution.

### 3. Same binary, license-gated — no open-source bait-and-switch fork

- There is **no fork** of OSS product logic. Community and Enterprise are the
  **same Go binary**. Enterprise features are switched on by a valid
  `LICENSE_KEY` (plus tenant configuration); with **no license, the binary runs
  in full community mode** and every OSS feature is available.
- The license check validates a **signed token offline** against an embedded
  public key — **no phone-home**, so air-gapped community and enterprise installs
  both work with no outbound network.
- Disabling or removing the license gate does not unlock any OSS capability,
  because every OSS capability is already on in community mode. The gate only
  ever guards the *additive* enterprise features.

### 4. The one-way import rule

- `versus-enterprise` (private) **imports** `versus-incident` (this repo) as a
  Go module. The dependency arrow points **one way only**.
- The OSS repository **never** imports the enterprise module, and **no enterprise
  symbol, type, or string ever leaks into this public tree.** OSS CI builds and
  passes without the enterprise module present.
- Enterprise features attach exclusively through **generic, unopinionated OSS
  seams** (e.g. `storage.Provider`, middleware, the admin router, and a small set
  of extension hooks added to OSS first). If the enterprise tier needs a new hook,
  the hook is contributed to OSS as a generic extension point — never as
  enterprise-specific code.

### 5. How this commitment is enforced

- Every PR is reviewed against this section. A change that removes, paywalls, or
  relicenses an existing OSS capability is rejected on sight.
- Any new feature proposed as Enterprise-only must pass the boundary test in §2
  and be recorded as *new* (not migrated). The Open-Core Strategist owns the
  written tiering verdict for contested features.
- The one-way import rule is checked by OSS CI building without the enterprise
  module.
- Changing **this** commitment requires the same explicit lead-maintainer
  approval as a breaking change (see below), and any narrowing of the OSS surface
  is out of bounds by construction.

## Changes to this document

Changes to `GOVERNANCE.md` follow the same lazy-consensus process as code
changes, but require explicit approval from the lead maintainer.
