package breaks

import (
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func IsImagePullReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName", "ImageInspectError", "RegistryUnavailable", "SignatureValidationFailed":
		return true
	}
	return false
}

func IsConfigErrorReason(reason string) bool {
	switch reason {
	case "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "PreCreateHookError", "PreStartHookError", "PostStartHookError":
		return true
	}
	return false
}

func isEvictionDisruption(reason string) bool {
	switch reason {
	case "PreemptionByScheduler", "DeletionByTaintManager", "EvictionByEvictionAPI":
		return true
	}
	return false
}

func evicted(record index.Record) bool {
	if record.Phase == "Failed" {
		switch record.Summary["pod_reason"] {
		case "Evicted", "Preempting", "NodeLost", "Shutdown", "NodeShutdown":
			return true
		}
	}
	return record.Summary["disruption_status"] == "True" && isEvictionDisruption(record.Summary["disruption_reason"])
}

func healthyAncestor(record index.Record, byUID map[string]index.Record) bool {
	seen := map[string]bool{record.UID: true}
	for depth := 0; depth < 16; depth++ {
		owner, ok := controllingOwner(record.Owners)
		if !ok || owner.UID == "" || seen[owner.UID] {
			return false
		}
		if owner.Namespace == "" {
			owner.Namespace = record.Namespace
		}
		next, exists := byUID[owner.UID]
		if !exists || owner.Namespace != record.Namespace || next.Namespace != record.Namespace || next.Kind != owner.Kind {
			return false
		}
		seen[owner.UID] = true
		switch next.Kind {
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
			if availabilityKnown(next) && next.Replicas > 0 && next.Available >= next.Replicas {
				return true
			}
		}
		record = next
	}
	return false
}

func timestamp(record index.Record, key string) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, record.Summary[key])
	return at
}

func warningReason(reason string) string {
	switch reason {
	case "BackOff", "Unhealthy", "ProbeWarning", "Failed", "FailedScheduling", "FailedMount", "FailedAttachVolume", "FailedCreate", "FailedCreatePodSandBox", "FailedKillPod", "FailedPostStartHook", "FailedPreStopHook", "Evicted", "Preempting", "OOMKilling", "SystemOOM", "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InspectFailed", "NodeNotReady", "NetworkNotReady", "ProgressDeadlineExceeded", "BackoffLimitExceeded", "DeadlineExceeded":
		return reason
	}
	return "Other"
}

func signalName(signal int32) string {
	switch signal {
	case 6:
		return "SIGABRT"
	case 9:
		return "SIGKILL"
	case 11:
		return "SIGSEGV"
	case 15:
		return "SIGTERM"
	}
	if signal > 0 && signal <= 64 {
		return strconv.Itoa(int(signal))
	}
	return ""
}

func exitHint(code int32) string {
	switch code {
	case 126:
		return "not_executable"
	case 127:
		return "not_found"
	case 134:
		return "SIGABRT"
	case 137:
		return "SIGKILL"
	case 139:
		return "SIGSEGV"
	case 143:
		return "SIGTERM"
	}
	return ""
}
