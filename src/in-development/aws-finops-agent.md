# AWS FinOps Agent

_In development — not released yet_

**See what your AWS bill is, what moved it and why, who owns each part, and
what you can save — in words a manager can read.** The FinOps agent reads your
AWS cost data once a day (read-only) and turns it into one page and a two-page
report you can send to your boss.

> **Preview only.** Everything on this page is from the design preview and uses
> sample data. The FinOps agent is not part of a release yet, so you cannot
> install or enable it today. Screens, wording and limits may still change.

## What it is

The FinOps agent is part of [AWS Harness](./aws-harness.md). This first step is
**read-only**: it watches your cost, finds what changed, explains it and
reports it. It never changes anything in AWS.

It is written for two readers at once:

- **Managers and finance** read the totals, the forecast and the plain-language
  reasons, and export the PDF.
- **Engineers** switch to **Details** for the technical labels, usage types and
  resource IDs, and follow each increase to the change that started it.

The **Summary | Details** switch changes labels and columns, never the numbers.

Two short guides go with this page:
[Read your FinOps report](./aws-finops-report.md) and
[Tag your AWS cost for ownership](./aws-finops-tagging.md).

## The questions it answers

- **How much did we spend, and is that OK?** Total, forecast and, if you set
  one, the budget.
- **What went up or down?** One chart from last month's total to this month's,
  one row per cause.
- **Why did it go up?** Each increase is linked to the change that most likely
  started it, with who made it and when.
- **Who owns it?** The team from your cost tag, or the account owner. Cost with
  no owner is shown as **Unallocated**, never hidden. See
  [Tag your AWS cost for ownership](./aws-finops-tagging.md).
- **Is it expected?** An admin can mark an increase expected, with a reason and
  an end date.
- **What can we save, and how?** A savings list with the monthly amount, effort,
  risk and a step-by-step guide.
- **Can I send this to my boss?** PDF, CSV, a copyable summary and a frozen
  monthly report.

## A quick tour

### The FinOps page

![AWS FinOps page: three cards for October cost, change vs September and forecast for November, a What moved the cost chart with one row per driver, and Spend by service, Spend by account and Savings found cards below](../docs/images/aws-finops-page.png)

The page opens with **three cards**:

| Card | Shows |
|---|---|
| **October cost** | The month's total, with a small line that shows when it moved |
| **Change vs September** | The amount and percent, and the word **Up**, **Down** or **About the same** |
| **Forecast November** | The expected total. If you set a budget, one quiet line says whether the forecast is over it |

The cost shown is **what you actually pay**: after discounts, credits and
Savings Plans. The ⓘ on each card explains the source and the data lag.

**What moved the cost** is the heart of the page. It starts with last month's
total, adds or subtracts one row per cause, and ends with this month's total.
Each row shows the AWS service, the owner, a short reason and its status
(**Open** or **Expected**). Click a row to see the full explanation.

**Exact and estimated.** Totals per account, service, team and region are
always exact. Cost Explorer can only split cost two ways in one request, so a
figure that combines more than that — for example a team's share of one service
across several accounts — is shared out in proportion. Only those figures carry
a **≈** sign and an ⓘ (*Estimated split*), and their sentences say *likely*.

Below it:

- **Spend by service** — the top services with amount and share.
- **Spend by account, region or team** — switch between the views. A view is
  only offered when it actually splits the cost.
- **Savings found** — the only place the savings total appears, each item with
  its monthly amount, effort, risk and a **Guide**.

How the savings total is counted:

- **One action per resource.** If a resource has several possible savings (for
  example, remove it or make it smaller), the largest one counts. The others
  are listed under it as alternatives and are not added.
- **Commitments stand apart.** A Savings Plan is priced on today's usage,
  before your other savings shrink it. It is shown on its own as *up to $X a
  month, before other savings* and is never added to the total.
- **Checks that could not run are named.** For example, *Not checked: private
  endpoints — needs usage detail (Organization)*. They are never shown as $0.

Alerts go through your existing Versus channels (Slack, Teams or email):

- **Increases.** A cost line that stays up for three complete days and adds at
  least $100 a month raises one alert. It alerts again only if the impact
  doubles.
- **Budgets.** If you set a budget, you get one alert when it turns **At risk**
  (the forecast is above 90% of the budget) and one more if it goes **Over
  budget**. Each happens at most once a month.

### One increase, explained

![AWS FinOps increase page: network traffic in Production went up $1,980 in October, with a daily cost chart before and after Oct 9, where the increase came from, the likely change that started it, the owner and a fix that saves about $1,520 a month](../docs/images/aws-finops-why.png)

Clicking a row opens the full story for that increase:

- **Before and after** — the daily cost with the day it started marked.
- **Where it came from** — the increase split by usage type and, when
  available, by resource.
- **Why** — up to three changes that could have started it, each labelled
  **Likely** or **Possible**, with who made it (for example, a deploy pipeline)
  and links to Changes and the Map. When several changes fit equally well,
  none is singled out: all are **Possible**, closest in time first. If
  resource-level data is off, the page suggests turning it on to narrow the
  list down.
- **No guessing.** **No change found in the 48 h before** appears only when the
  change history covers that whole window. If it does not, the card says
  **Change history incomplete for that window** instead.
- **Who** — the owner and the rule that picked them, such as the `team` tag.
  The owner is the team with the most cost on that line since it started.
  Untagged cost counts as **Unallocated** and can be the owner.
- **Fix** — the matching saving, with the monthly amount, effort, risk and
  guide.

From here you can **Mark expected** (admins only; a reason and an end date of
up to 90 days are required, and it reopens by itself if the cost is still
high), open an issue, copy a summary or ask AI to explain it.

### A report for managers

![AWS FinOps PDF report for October 2026 on two pages: the three summary cards and What moved the cost with a short reason under the top rows on page one; spend by service and account, savings found, expected increases and About this report on page two](../docs/images/aws-finops-report.png)

**Export → PDF** opens a print layout and your browser's **Save as PDF**. It
works offline and fits on two pages:

1. The three cards and **What moved the cost**, with one plain sentence on why
   under each of the top three rows.
2. Spend by service and account, the savings found, expected increases with
   their reasons, and **About this report** — which accounts it covers, the cost
   measure and the last complete day.

Until AWS closes the month's bill on the 5th, the report is marked **Not
final**. [Read your FinOps report](./aws-finops-report.md) walks through it
block by block.

Other exports:

| Export | What you get |
|---|---|
| **CSV** | The table you are looking at, with your filters |
| **Copy summary** | A few plain lines (the total, the top three causes, the savings) to paste into Slack or email |
| **Save as monthly report** | Freezes the month. The PDF of a frozen report shows when it was frozen, so the numbers your boss saw never shift |
| **Slack summary** | A monthly picture sent through your existing Versus report delivery |

### Ask the DevOps agent

You can ask about cost in plain language, such as *"Why did AWS cost go up in
October?"*, from **Ask AI** on the page, in web **Chat**, or in the
`versus-devops` command line tool. You get the same answer in each place.

The agent uses these read-only tools:

| Tool | What it answers |
|---|---|
| `cost_breakdown` | Total, change, forecast and spend by service, account, account group, region or team |
| `cost_drivers` | What moved the cost between two periods, by cause, with owners |
| `cost_increases` | Detected increases with start day, monthly impact, owner and status |
| `cost_increase_detail` | One increase: where it came from, the change that likely started it, the owner rule and related savings |
| `savings_opportunities` | Savings found, with monthly amount, effort, risk and owner |
| `budget_status` | Each budget against month-to-date cost and the forecast: on track, at risk or over budget |
| `cost_accounts` | The accounts behind the bill, with status, group, cost, change, top driver and top team |
| `get_aws_connection` | The AWS connection: each account's status and freshness, denied services with the fix, and gaps |

- **Versus computes every number.** The AI only explains the results and links
  to the exact rows it used. It never calculates or invents a figure or a cause.
- **Asking never costs a paid AWS request.** The tools read only the cost data
  and connection state Versus already stores. They never call AWS. If an answer
  needs data Versus has not fetched, the tool says so and links to the page.
- **The agent only reads.** Marking expected, dismissing a saving, budgets, the
  paid refresh and exports stay in the page; the agent links you there.

See the [DevOps Agent](../agent/devops-agent.md) for how Chat works today, and
the [Tool Reference](../agent/tools/overview.md) for the tool catalog.

### Light theme

![AWS FinOps page in the light theme, with the same cards, chart and lists as the dark theme](../docs/images/aws-finops-page-light.png)

The page works in light and dark themes. The PDF always prints light.

## What we plan to release

| Capability | Open source | Enterprise |
|---|---|---|
| FinOps page: what moved the cost, increases, savings, budgets and forecast | Yes | Yes |
| Alerts for new increases through your existing Versus channels | Yes | Yes |
| PDF and CSV export, copy summary, frozen monthly reports | Yes | Yes |
| Monthly Slack summary | Yes | Yes |
| Ask AI, the DevOps agent and MCP (read-only) | Yes | Yes |
| The whole bill for an AWS Organization through the management account, with account names, account groups from your config and the Accounts tab | Yes | Yes |
| Cost history | 13 months | Longer than 13 months |
| PDF report sent by email on a schedule | — | Yes |
| Branded PDF made on the server | — | Yes |
| Chargeback: splitting shared costs across teams | — | Yes |
| Finance-only roles, and limiting which accounts' cost each team can see | — | Yes |
| Grouping by organizational unit (OU) from AWS Organizations, and budgets per OU | — | Yes |
| Connecting cost-only accounts in bulk with StackSets | — | Yes |
| Scheduled export of cost data to S3 or a SIEM | — | Yes |

The rule: open source answers **"why did it go up?"** and lets everyone send the
PDF. Enterprise adds automation and governance, not different answers.

## What it reads and what it costs

- **Off by default.** Cost reads are opt-in. An admin turns them on in Settings.
- **Read-only.** The FinOps agent uses the same small read-only role as AWS
  Harness, with Cost Explorer, AWS Budgets and cost recommendation reads added.
  It never changes AWS.
- **Savings are guides only.** Each one gives the console path or a snippet you
  run yourself. Versus never applies a saving.
- **Once a day.** Cost data refreshes once a day (09:00 by default). AWS cost
  data itself lags by up to 24 hours, so more refreshes would not show anything
  new. An admin can press **Refresh now** once a day.
- **One replica refreshes.** With Postgres, Versus must reach the database
  directly or through a session-mode pooler. See
  [Connection pooling and AWS cost refresh](../configuration/postgres-storage.md#connection-pooling-and-aws-cost-refresh).
- **When a day counts.** A day is complete 24 hours after it ends. Later days
  are drawn dashed and are left out of comparisons and increase checks.
- **A hard limit on AWS charges.** AWS charges $0.01 for each Cost Explorer
  request. Versus makes **at most 20 paid requests a day** — about **$0.20 a
  day**, or around $6 a month at most. If a day's plan would go over, it skips
  the least important reads and the page names what it skipped.
- **Looking deeper costs a request.** Opening more detail for one account and
  service in an Organization spends one paid request. Only admins can do it,
  and it never uses the requests the next scheduled refresh needs.
- **No billing account details.** It never reads invoices, payment methods or
  billing account settings. Those permissions are blocked in the role.
- **Management account: cost only.** If you connect the management (payer)
  account of an AWS Organization, it gets a separate **cost-only** role with no
  access to anything else in that account.
- **Your currency.** Amounts are shown in your bill's currency and never
  converted.

## How the page shows data in every case

Cost data is often incomplete: an account is not readable, today is not
finished, tags are missing. The page follows three rules:

1. **Missing is never zero.** A gap is shown as a gap, never as $0.
2. **Every total says what it includes.** For example, *2 of 3 accounts*.
3. **Every gap says why and how to fix it**, on the same line.

![Overview sheet of fifteen FinOps states when data is missing, each with what happened, what you see and what you can do, ending with the three rules: missing is never zero, every total says what it includes, every gap says why and how to fix it](../docs/images/aws-finops-states.png)

Each case below shows the full page.

### Getting started and access

#### Cost reads are off

![FinOps page with cost reads off: what FinOps answers, the six daily calls it would make, a charge of at most $0.20 a day, the IAM policy to copy and a Turn on button](../docs/images/aws-finops-state-01-cost-off.png)

This is the default. Instead of empty charts, the page explains what it would
read, what that costs and the policy it needs. Admins press **Turn on**;
everyone else sees **Ask an admin**.

#### One account's cost is not readable

![FinOps page where the Staging account's cost access is denied: totals marked 2 of 3 accounts and one Staging row with a Copy fix button](../docs/images/aws-finops-state-02-account-not-readable.png)

Every total is marked **2 of 3 accounts**, and the change compares the same two
accounts. The missing account has one row with **Copy fix**; its cost fills in
at the next daily refresh.

#### Cost Explorer is not available

![FinOps page for an account where Cost Explorer is not available: one card listing the likely causes in order, each with one action, and no totals](../docs/images/aws-finops-state-17-cost-explorer-unavailable.png)

If Cost Explorer has never been opened in the account, or the Organization
admin has turned it off for member accounts, there is no cost to show. One card
lists the likely causes in order, each with its fix: open Cost Explorer once,
ask the Organization admin, or read cost through the management account. The
other AWS pages keep working.

### Freshness

#### The refresh failed

![FinOps page marked Stale since Nov 1 after AWS throttled the refresh, with the last two days drawn hatched as Not loaded](../docs/images/aws-finops-state-03-refresh-failed.png)

If AWS throttles or fails the daily refresh, the page keeps the last good data,
marks it **Stale since** that day and draws the missing days as **Not loaded**.
The next scheduled refresh fills them in.

#### The daily request limit is reached

![FinOps page after the 20 paid request limit was reached: the forecast and discount coverage are marked Skipped today while every other number is shown](../docs/images/aws-finops-state-04-request-cap.png)

The page names exactly what was skipped today (here, the forecast and discount
coverage) and shows everything else. Wait for tomorrow, or raise the limit in
Settings.

#### Last month's bill is not closed yet

![FinOps page on Nov 4 with the October total tagged Not final and Save as monthly report locked until Nov 5](../docs/images/aws-finops-state-11-bill-not-closed.png)

AWS closes the previous month's bill on the 5th. Until then the total is tagged
**Not final**, exports are marked as drafts, and **Save as monthly report**
unlocks on the 5th, so a frozen report matches the invoice.

#### Today is only partial

![FinOps daily cost chart for November with complete days to Nov 3 and Nov 4 drawn as a dashed bar marked partial](../docs/images/aws-finops-state-12-partial-day.png)

A day counts as complete 24 hours after it ends. The last complete day is
named, and later days are drawn dashed and kept out of the total. Increases
never fire on a partial day, so nothing alerts early.

### Short history

#### A new account

![FinOps page for an account connected on Oct 25: the total names the days it covers, What moved the cost starts in November, and increases show Learning](../docs/images/aws-finops-state-07-new-account.png)

Totals, spend and savings work from day one. The total says which days it
covers. Comparisons start with the first full month, and increase detection
shows **Learning** for the first 14 days.

#### The forecast is an estimate

![FinOps forecast card in a dashed outline tagged Estimate, calculated from the last 7 complete days](../docs/images/aws-finops-state-15-forecast-unavailable.png)

With too little history for the AWS forecast, Versus estimates from the last
seven complete days and labels it **Estimate**. With fewer than seven days it
says **Not enough history**, never $0.

### Ownership

#### No team tag

![FinOps page with no team tag in cost data: no Owner column, one line saying Owner: no team tag with Set up tags, and Team locked in Spend by](../docs/images/aws-finops-state-05-no-team-tags.png)

Your `team` tag must be activated as a cost allocation tag in AWS Billing.
Until it is, the Owner column is hidden and one line says why, with **Set up
tags**. Owners appear from the day you activate it; earlier cost stays without
a team.

#### The same tag on everything

![FinOps page where all cost carries team=platform: the Owner column is hidden and one line explains why](../docs/images/aws-finops-state-14-same-tag.png)

If every cost line has the same team, an Owner column would say the same thing
on every row. It is hidden, one line says why, and spend stays by account.

### What moved

#### Cost went down

![FinOps page where October cost fell $2,310: the change card says Down in green and the decreases lead What moved the cost](../docs/images/aws-finops-state-06-cost-down.png)

Same layout, no alarm colour. **Down** shows in green and the decreases lead
the chart, so you can see what saved money and keep it.

#### About the same

![FinOps page where October cost is About the same, +0.4%, with the ups and downs that offset each other listed in What moved the cost](../docs/images/aws-finops-state-13-cost-flat.png)

Within ±2% the card says **About the same**. The rows that cancelled each other
out stay visible. In a very quiet month there is one line: *No driver above
$50; the cost was steady*.

#### No change found

![FinOps What moved the cost where one increase says No change found and another says no change history yet, with amount, service, owner and status still shown](../docs/images/aws-finops-state-08-no-change-found.png)

When change history covers the whole window and nothing changed near the
start, the row says **No change found**. When the history is not complete for
that window, it says **Change history incomplete** instead, so a gap never
looks like a clean result. Versus never guesses a reason. The amount, service,
owner and status are still shown.

#### Credits and one-time fees

![FinOps What moved the cost with an annual support fee marked One-time, no alert, and a $500 credit, each as its own outlined row](../docs/images/aws-finops-state-10-credits-fees.png)

Credits, refunds and fees get their own rows. A one-time charge such as an
annual support fee is never counted as an increase and never raises an alert.

#### Resource-level data is off

![FinOps increase page where the cause tree stops at usage type, each branch marked Resources: not available, with a Turn on link](../docs/images/aws-finops-state-09-resource-data-off.png)

Without Cost Explorer's resource-level data, the breakdown on the increase page
stops at usage type and says so. Why, Who and Fix still work. You can turn the
data on in AWS (AWS may charge for it).

### Account shapes

#### One account

![FinOps page for a single account: Account is a plain label and Spend by opens on Region with no Account option](../docs/images/aws-finops-state-16-single-account.png)

The account filter becomes a plain label and **Spend by** opens on Region.
Nothing says *1 of 1* or *100%*. Connect a second account and the account views
come back.

#### One account and one region

![FinOps page for one account, one region and no team tag: labels instead of filters, no Spend by card, and Spend by service showing what is inside each service](../docs/images/aws-finops-state-18-single-region.png)

When nothing splits the cost, there is no **Spend by** card at 100%. Instead,
**Spend by service** widens and shows what is inside each service, such as
instances, volumes and IP addresses.

#### One region with team tags

![FinOps page for one account and one region with team tags on four teams: Team is the only filter and a Spend by team card includes Unallocated](../docs/images/aws-finops-state-19-single-region-tagged.png)

Team is the only split, so the card is simply **Spend by team**, including the
**Unallocated** share with **Find untagged** to close the gap.

#### An AWS Organization

![FinOps page reading 48 accounts through the management account: an Account column in What moved the cost, cost-only accounts with Connect links, an Unused Savings Plan row and spend by account group](../docs/images/aws-finops-state-20-organization.png)

Connect the management account and the page shows **the whole bill**, so the
total matches the invoice. Accounts without the Versus role are marked **Cost
only**: their cost is known, but the reason says **Not connected** with
**Connect**. Organization-level items such as an unused Savings Plan get their
own rows. Spend can be grouped by account groups you define in config.

With many accounts, some cross figures are estimated splits and carry **≈**.
Some savings checks need usage detail that is not read for every account, so
they show as **Not checked … needs usage detail (Organization)**. An admin can
open more detail for one account and service; that spends one paid request.

#### The Accounts tab

![FinOps Accounts tab: a table of 48 accounts grouped with subtotals, each with status, cost, change, top driver, top team and a 30-day trend](../docs/images/aws-finops-state-21-organization-accounts.png)

With ten or more accounts, an **Accounts** tab lists every account with its
status, group, cost, change, top driver, top team and a 30-day trend. Sort by
change, filter cost-only or new accounts, and export to CSV.

## Status

The FinOps agent is **in development** as part of AWS Harness and is not in a
release yet. There is no release date, and the screenshots on this page come
from the design preview.

This first step only reads. If actions on AWS are added later, they will only
run from the `versus-devops` command line tool with your own AWS credentials,
never from the Versus server or the web page — and deleting resources will never
be one of them. We will announce availability in
[Releases](https://github.com/VersusControl/versus-incident/releases) when it is
ready.
