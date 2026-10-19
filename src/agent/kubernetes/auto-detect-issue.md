# Automatically Detect Kubernetes Issues

Use **Issues** to find affected Kubernetes resources, then **Timeline** to examine changes, crashes, and logs around the break. These views help you choose what to investigate; a nearby change is not proof of the cause, and investigation does not automatically fix the workload.

Open **Kubernetes** and select the cluster and namespace you want to investigate. If the cluster is not available, see [Configuration](configuration.md). For more than one cluster, see [Multi Cluster](multi-cluster.md).

The screenshots use demonstration data: in the `shop` namespace, the **Deployment** `checkout` is in CrashLoopBackOff, with OOMKilled terminations and restarts. An image update from `v41` to `v42`, a ConfigMap change, and a scaling change precede the break.

## Triage In Issues

Open **Issues** to review the severity, resource kind, namespace/name, issue description, affected count, and break or first-seen time. Start with critical issues, then examine warnings such as unavailable Pods or a stalled rollout. Severity prioritizes the detected condition; it does not establish the business impact.

![Issues list with critical checkout and other affected workloads](../../docs/images/kubernetes-auto-detect-issues-overview.png)

*Issues brings the affected workloads together before you open individual Pods.*

Set **Severity** to **All severities**, **Critical**, **Warning**, or **Info**, and enter a **Namespace** to narrow the list. Check the selected cluster before interpreting counts. A grouped workload row summarizes affected resources: `7 of 7 pods affected` does not mean seven separate Deployments or seven crash occurrences. The severity badges summarize issue groups, while the row's affected count describes resources within a group. Do not add overlapping evidence counts to estimate the number of incidents. Use **Prev** and **Next** below the list to page through results; an empty filtered list is not a cluster-wide health check.

### Expand The Evidence

Expand `checkout` with the row's chevron. Review the preceding changes and how long before or after the break each was recorded. On a narrow screen, **more changes** opens the focused Timeline instead of listing every change in the row.

Read the crash-reason badges alongside the issue description. **CrashLoopBackOff** describes repeated failure to stay running; **OOMKilled** records an out-of-memory termination. An `Error` termination or an exit code alone is not an OOM diagnosis. In particular, exit `137` by itself does not prove the termination signal or cause.

![Expanded checkout issue showing preceding changes and crash evidence](../../docs/images/kubernetes-auto-detect-issues-details.png)

*The expanded row connects workload identity, change timing, and crash evidence without declaring a cause.*

**Lifetime restarts** are cumulative counters, not crashes within the selected period. Termination badges summarize the reported evidence; warning-event counts are not restart counts. Expand **Warnings** when present: reconstructed history, gaps, untracked resource types, stale data, or partial evidence limit what you can conclude. A row with no recorded change is not proof that nothing changed.

For the checkout incident, use the OOMKilled evidence to investigate memory pressure or consumption, while checking whether the new image or configuration changed workload behavior. If resource **Events** show a failed readiness or liveness probe, inspect its message and timing too. A failed probe can explain readiness loss or a restart, but is not interchangeable with an OOMKilled reason. Do not infer a failed probe just from CrashLoopBackOff.

## Reconstruct The Incident In Timeline

Open **Timeline**, or use **Full timeline** from the overview. Choose **15m**, **1h**, **6h**, or **24h**, then use **Namespace** and **Kind** to narrow the history. Widen the period when the rollout began before the visible window. The unfocused recent view advances with time; opening an issue's **Timeline** instead centres the view on its break.

For a broken-workload focus, use **Timeline** on that workload's issue row. This includes available related resource changes, not just changes to the Deployment itself. Use **Clear root filter** (the cross beside the **Filtered** resource) to return to the broader view. Keep the namespace and cluster in view so similarly named resources are not confused. **Prev** and **Next** page through recorded changes within the chosen period; they do not move the time window.

![Timeline overview with separate resource lanes and change markers](../../docs/images/kubernetes-auto-detect-timeline-overview.png)

*Resource lanes show when changes occurred across the selected scope.*

### Read Lanes And Markers

Each lane identifies a resource kind, name, and namespace. Change markers, crash markers, the **break** line, and **now** share a time axis. Select a marker or its label to inspect details; closely spaced events may be grouped together.

In the focused checkout view, the highlighted **suspect window** runs from 30 minutes before the break to two minutes after it. It is an investigation aid, not a statement that every change inside it caused the failure. A ConfigMap or Secret marker records that it changed; its content is not displayed.

![Focused checkout Timeline with break line, suspect window, and crash markers](../../docs/images/kubernetes-auto-detect-timeline-focus.png)

*Focus on checkout to compare the image and configuration changes with the recorded break and terminations.*

An image-change marker can show `v41` to `v42`. Open its details and **Open resource** to inspect the resource's **Timeline** history. Compare the recorded fields' before/after values with the last known healthy version. These are selected fields, not a complete historical manifest or a patch to apply; unlisted fields and hidden configuration content may also differ.

![Resource change comparison for the checkout image update](../../docs/images/kubernetes-auto-detect-change-compare.png)

*Compare the recorded image change before deciding whether the rollout needs further investigation.*

### Inspect A Crash

Select a crash marker to see the container, Pod, termination reason, exit code, and time. **Open pod** opens that specific Pod. **Logs before crash** selects that Pod, switches to **Previous container**, and sets the log window to the five minutes ending at the selected crash.

![Crash popover with Logs before crash and Open pod actions](../../docs/images/kubernetes-auto-detect-crash-actions.png)

*Go directly from a termination to the previous container's available output.*

If you need more detail, expand **Captured crash evidence** to check when the crash happened, when it was observed, and what history is available. Use **Previous** and **Next** to page through observations. Timeline crash markers show terminations from the current page of available history within the chart window, not every past crash. Crashes can occur between observations; lifetime restart counters do not fill those gaps.

## Read Logs Around The Break

In a focused workload Timeline with an available break time, **Logs at break** shows a bounded window, initially five minutes before through two minutes after the break. The **Log lines** lane shows per-minute volume with errors in red. Use it to locate a burst, then read the actual messages before drawing a conclusion.

Drag either end of the **log window** bracket to narrow or widen the interval. The handles also work with arrow keys in 30-second steps. The window is bounded between 30 seconds and two hours. A window extending beyond now can refresh as that interval becomes available; it is not evidence about future workload health.

![Logs at break toolbar with Pod, container mode, severity, and search filters](../../docs/images/kubernetes-auto-detect-log-filters.png)

*Narrow the logs to the Pods, container output, and messages relevant to the failure.*

| Control or detail | How to use it | What it helps answer |
| --- | --- | --- |
| **All pods** and Pod chips | Select one or more Pods, or return to all available Pods. | Is the failure isolated to one replica? |
| **Merged**, **Previous container**, **Current** | Choose the available container output to examine. | Did the message come before the restart or from the replacement container? |
| **Errors** and **Warnings** | Toggle the severity filter; select it again to clear it. | Which messages deserve a closer look? |
| **Search in window** | Search within the selected log interval. Clear it to restore other messages. | Are timeout, memory, or startup messages present? |
| **Top messages in window** | Review repeated message patterns and their counts. | Is one failure repeated, or are there several distinct symptoms? |
| **Load more logs** | Load the next page with the current window and filters. | Are matching lines still waiting to be loaded? |
| **Expand logs** | Open the larger log viewer. | Do long messages need more room? |

The footer distinguishes total lines in the window from matching lines loaded. **Load more logs** does not move the window earlier. If a page expires or filters invalidate it, use **Reload logs**. Clear Pod, mode, severity, and search filters before treating an empty result as an absence of messages.

Read **Source** and **Coverage** with the messages. Output may come from **Captured at break**, **Kubernetes container logs**, or a linked source. Check each row's Pod, container, and source before comparing messages across replicas or restarts. Use Timeline markers to compare timing, then verify the suspected cause against the messages.

### Handle Missing History

Open **Log coverage tip** when the lightbulb appears. It explains why part of the requested interval is unavailable, such as repeated restarts, a deleted Pod, rotated logs, a late capture, or missing read permission. Kubernetes may retain only the last previous container's output; selecting an older crash cannot recreate logs that are no longer available.

![Logs at break with the coverage tip explaining unavailable history](../../docs/images/kubernetes-auto-detect-break-logs.png)

*Check the coverage tip before treating missing messages as evidence that nothing failed.*

The tip's **Link a source in Agent tab** opens this cluster's source settings. Check the workload scope and use **Test link** to inspect the preview; a successful preview does not guarantee complete incident history. For setup, see [Configuration](configuration.md) and the [Kubernetes connector guide](../connectors/kubernetes.md).

![Agent tab with linked log source settings and Test link preview](../../docs/images/kubernetes-auto-detect-agent-link.png)

*The Agent tab is a secondary step when the incident window needs an additional log source.*

Learning toggles save settings but do not currently affect runtime behavior or recover missing logs.

Missing history or no matching lines do **not** prove that the workload was healthy. Record the missing interval and check other evidence before ruling out a failure.

## Check The Workload Now

Use **Open live logs** to open the workload drawer's **Logs** tab. The workload **Live** view reads the last hour, up to 500 lines per Pod: it is a bounded snapshot, not a continuously streaming tail. Choose **Previous container**, **Container**, or **Search** as needed and use **Refresh logs** for a new snapshot. Read any truncation, omitted-Pod, or partial-failure warnings. Compare current startup or error output with the historical break window; do not confuse this view with separate Pod streaming controls.

![Workload Logs drawer with the Live snapshot selected](../../docs/images/kubernetes-auto-detect-live-logs.png)

*Compare recent workload output with the messages captured around the break.*

Use **Ask chat** from **Logs at break** to carry the workload and selected time window into an investigation. For example: "Compare checkout's OOMKilled evidence with the v41 to v42 image change and available logs in this window. What should I verify next, and what evidence is missing?" This supplies investigation context; it does not guarantee a root-cause finding or apply a fix.

## Verify Recovery

After your team makes a reviewed remediation through its normal deployment process:

1. Return to the same cluster and namespace in **Issues**. Confirm that current evidence is fresh and the affected workload is no longer reported broken; do not rely only on a lower filtered count.
2. Open the resource and check ready/available Pods and **Events**. For a rollout, confirm the intended image and expected replicas. Check whether new probe failures, waiting reasons, or terminations are still appearing.
3. Revisit **Timeline** with a period covering both the change and the recovery checks. Inspect recent crash observations and their coverage. A quiet or incomplete chart alone is insufficient.
4. Compare recent workload **Live** log snapshots with **Logs at break**. Check for recurring errors and renewed restarts. Lifetime restart counters need not return to zero; look for further increases instead.
5. Verify checkout behavior with your normal application checks. Kubernetes readiness and quiet logs do not, by themselves, prove that customer checkout is working.

Keep the distinction between what was observed, what may explain it, and what you verified after remediation. This makes the investigation useful even when some historical evidence is unavailable.
