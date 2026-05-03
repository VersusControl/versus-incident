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

## Changes to this document

Changes to `GOVERNANCE.md` follow the same lazy-consensus process as code
changes, but require explicit approval from the lead maintainer.
