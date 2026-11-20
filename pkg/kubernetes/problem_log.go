package kubernetes

import (
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// problemLog limits the warnings for failures that are not tied to one
// inventory logger: credential checks, issue alert delivery, history saves,
// capture refusals, unmatched exclusions and the capture mode. Keys name the
// org, the cluster and what failed, never a message. It is process-wide so a
// successor service after a credential rotation shares it; tests swap in a
// throttle with their own clock.
var problemLog atomic.Pointer[logging.Throttle]

func init() { problemLog.Store(&logging.Throttle{}) }

// problems returns the process-wide warning throttle.
func problems() *logging.Throttle { return problemLog.Load() }

// problemKey names what failed for a throttle key: org, cluster and topic.
func problemKey(orgID, clusterID, topic string) string {
	return orgID + "/" + clusterID + "|" + topic
}

// problemCause maps a failure class to the cause a user reads and what to
// check. ok is false for a class that needs no action: the kind is read inside
// namespaces by design, it is still syncing, or the failure heals on the next
// cycle (a lost lease, a write conflict, API throttling, a cancelled request).
// perKind words the hint for named kinds rather than the whole cluster.
func problemCause(class string, perKind bool) (cause, check string, ok bool) {
	switch class {
	case "forbidden":
		if perKind {
			return "forbidden", "grant list on these kinds to the reader role, or add them to exclude_resources", true
		}
		return "forbidden", "grant the reader role the Kubernetes RBAC the connector documents", true
	case "unauthorized", "credential_rejected":
		return "credential rejected", "restore or rotate the credential configured for this cluster", true
	case "credential_unavailable":
		return "credential unavailable", "check the token file, kubeconfig or cloud identity this cluster uses", true
	case "token_endpoint_unavailable":
		return "token endpoint unreachable", "check that Versus can reach the cloud token endpoint (DNS, egress, proxy); Versus keeps retrying and connects when it answers", true
	case "unavailable":
		if perKind {
			return "server error", "check the API server serves these kinds; an aggregated API or a conversion webhook behind them may be failing", true
		}
		return "unreachable", "check the cluster endpoint and the network path from Versus to the API server", true
	case "timeout", "collection_unavailable":
		return "unreachable", "check the cluster endpoint and the network path from Versus to the API server", true
	case "storage_unavailable":
		return "storage unavailable", "check the database (or data directory) Versus stores history in is reachable and has free connections and space", true
	case "storage_rejected":
		return "storage rejected", "read the history capture lines for the reason; restarting does not help", true
	case "record_limit":
		return "too many records", "narrow collection with namespaces or exclude_resources for this cluster", true
	case "record_rejected", "invalid_list_envelope", "invalid_json":
		return "invalid records", "check the API server, and any proxy in front of it, returns valid objects", true
	case "incomplete_pages":
		return "incomplete list", "check the API server, and any proxy in front of it, returns every page of a list", true
	}
	return "", "", false
}

// maxProblemKinds bounds how many kind names one warning lists.
const maxProblemKinds = 8

// problemKindList renders kind names as one bounded, safe field.
func problemKindList(kinds []string) string {
	sort.Strings(kinds)
	shown := kinds
	if len(shown) > maxProblemKinds {
		shown = shown[:maxProblemKinds]
	}
	tokens := make([]string, len(shown))
	for i, kind := range shown {
		tokens[i] = boundedLogToken(kind)
	}
	list := strings.Join(tokens, ",")
	if extra := len(kinds) - len(shown); extra > 0 {
		list += ",+" + strconv.Itoa(extra) + "_more"
	}
	return list
}
