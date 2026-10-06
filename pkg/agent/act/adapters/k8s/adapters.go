package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
)

const (
	RolloutRestart act.ActionType = "k8s.rollout_restart"
	Scale          act.ActionType = "k8s.scale"
	CronJobSuspend act.ActionType = "k8s.cronjob_suspend"
	CronJobResume  act.ActionType = "k8s.cronjob_resume"
	NodeCordon     act.ActionType = "k8s.node_cordon"
	NodeUncordon   act.ActionType = "k8s.node_uncordon"
	CronJobTrigger act.ActionType = "k8s.cronjob_trigger"
)

var resourceNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
var namespacePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
var proposalIDPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Options struct {
	Cluster             string
	MaxReplicas         int
	VerificationTimeout time.Duration
	PollInterval        time.Duration
}

type Adapter struct {
	api     API
	options Options
	action  act.ActionType
}

func NewAdapters(api API, options Options) []act.Adapter {
	if api == nil {
		return nil
	}
	if options.MaxReplicas < 1 {
		options.MaxReplicas = 100
	}
	if options.VerificationTimeout <= 0 {
		options.VerificationTimeout = 2 * time.Minute
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	return []act.Adapter{
		Adapter{api: api, options: options, action: RolloutRestart},
		Adapter{api: api, options: options, action: Scale},
		Adapter{api: api, options: options, action: CronJobSuspend},
		Adapter{api: api, options: options, action: CronJobResume},
		Adapter{api: api, options: options, action: NodeCordon},
		Adapter{api: api, options: options, action: NodeUncordon},
		Adapter{api: api, options: options, action: CronJobTrigger},
	}
}

func (adapter Adapter) Type() act.ActionType { return adapter.action }
func (Adapter) Destructive() bool            { return false }

func (adapter Adapter) Schema() map[string]any {
	if adapter.action == Scale {
		return map[string]any{"type": "object", "properties": map[string]any{"replicas": map[string]any{"type": "integer", "minimum": 1, "maximum": adapter.options.MaxReplicas}}, "required": []string{"replicas"}, "additionalProperties": false}
	}
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func (adapter Adapter) Validate(_ context.Context, proposal act.Proposal) error {
	if adapter.api == nil || proposal.Type != adapter.action || !validName(proposal.Target.Name) {
		return errors.New("invalid Kubernetes action target")
	}
	if proposal.Target.Cluster != "" && proposal.Target.Cluster != adapter.options.Cluster {
		return errors.New("action cluster does not match configured actor")
	}
	_, namespaced, ok := workloadResource(proposal.Target.Kind)
	switch adapter.action {
	case RolloutRestart:
		if !ok || !namespaced || !validNamespace(proposal.Target.Namespace) || len(proposal.Params) != 0 && !emptyObject(proposal.Params) {
			return errors.New("rollout restart requires a supported workload and empty parameters")
		}
	case Scale:
		if !ok || (proposal.Target.Kind != "Deployment" && proposal.Target.Kind != "StatefulSet") || !namespaced || !validNamespace(proposal.Target.Namespace) {
			return errors.New("scale requires a Deployment or StatefulSet")
		}
		var params struct {
			Replicas int `json:"replicas"`
		}
		if decodeStrict(proposal.Params, &params) != nil || params.Replicas < 1 || params.Replicas > adapter.options.MaxReplicas {
			return errors.New("replicas must be within the configured positive range")
		}
	case CronJobSuspend, CronJobResume:
		if proposal.Target.Kind != "CronJob" || !validNamespace(proposal.Target.Namespace) || len(proposal.Params) != 0 && !emptyObject(proposal.Params) {
			return errors.New("cronjob control requires a CronJob and empty parameters")
		}
	case CronJobTrigger:
		var binding cronJobBinding
		if proposal.Target.Kind != "CronJob" || !validNamespace(proposal.Target.Namespace) || decodeStrict(proposal.Params, &binding) != nil || binding.SourceUID == "" || binding.ResourceVersion == "" || binding.TemplateHash == "" {
			return errors.New("cronjob trigger requires a bound CronJob template")
		}
	case NodeCordon, NodeUncordon:
		if proposal.Target.Kind != "Node" || proposal.Target.Namespace != "" || len(proposal.Params) != 0 && !emptyObject(proposal.Params) {
			return errors.New("node scheduling control requires a cluster-scoped Node")
		}
	default:
		return errors.New("action type is not allow-listed")
	}
	return nil
}

func (adapter Adapter) Bind(ctx context.Context, proposal act.Proposal) (json.RawMessage, error) {
	if adapter.action != CronJobTrigger {
		return proposal.Params, nil
	}
	if proposal.Target.Kind != "CronJob" || !validNamespace(proposal.Target.Namespace) || !validName(proposal.Target.Name) || proposal.Target.Cluster != "" && proposal.Target.Cluster != adapter.options.Cluster || !proposalIDPattern.MatchString(proposal.ID) || !emptyObject(proposal.Params) {
		return nil, errors.New("cronjob trigger requires a CronJob and empty parameters")
	}
	jobSpec, uid, resourceVersion, err := adapter.readCronJob(ctx, proposal)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(jobSpec)
	return json.Marshal(cronJobBinding{SourceUID: uid, ResourceVersion: resourceVersion, TemplateHash: hex.EncodeToString(hash[:])})
}

func (adapter Adapter) DryRun(ctx context.Context, proposal act.Proposal) (string, error) {
	if adapter.action == CronJobTrigger {
		job, err := adapter.jobManifest(ctx, proposal)
		if err != nil {
			return "", err
		}
		if _, err := adapter.api.Do(ctx, http.MethodPost, cronJobPath(proposal), url.Values{"dryRun": {"All"}}, job); err != nil {
			return "", err
		}
		return adapter.effect(proposal), nil
	}
	path, patch, err := adapter.patch(proposal)
	if err != nil {
		return "", err
	}
	if _, err := adapter.api.Do(ctx, http.MethodPatch, path, url.Values{"dryRun": {"All"}}, patch); err != nil {
		return "", err
	}
	return adapter.effect(proposal), nil
}

func (adapter Adapter) Execute(ctx context.Context, proposal act.Proposal) (act.Result, error) {
	if adapter.action == CronJobTrigger {
		job, err := adapter.jobManifest(ctx, proposal)
		if err != nil {
			return act.Result{}, err
		}
		if _, err := adapter.api.Do(ctx, http.MethodPost, cronJobPath(proposal), nil, job); err != nil {
			return act.Result{}, err
		}
		return act.Result{Summary: adapter.effect(proposal)}, nil
	}
	path, patch, err := adapter.patch(proposal)
	if err != nil {
		return act.Result{}, err
	}
	if _, err := adapter.api.Do(ctx, http.MethodPatch, path, nil, patch); err != nil {
		return act.Result{}, err
	}
	encoded, _ := json.Marshal(map[string]string{"type": string(adapter.action)})
	return act.Result{Summary: adapter.effect(proposal), Metadata: encoded}, nil
}

func (adapter Adapter) Verify(ctx context.Context, proposal act.Proposal, _ act.Result) (act.Verification, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, adapter.options.VerificationTimeout)
	defer cancel()
	for {
		verified, err := adapter.verifyOnce(verifyCtx, proposal)
		if err != nil {
			return act.Verification{}, err
		}
		if verified {
			return act.Verification{Verified: true, Summary: "Kubernetes reports the requested state"}, nil
		}
		if verifyCtx.Err() != nil {
			return act.Verification{Summary: "Kubernetes did not report the requested state before the verification deadline"}, nil
		}
		timer := time.NewTimer(adapter.options.PollInterval)
		select {
		case <-verifyCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (adapter Adapter) patch(proposal act.Proposal) (string, []byte, error) {
	if err := adapter.Validate(context.Background(), proposal); err != nil {
		return "", nil, err
	}
	resource, namespaced, _ := workloadResource(proposal.Target.Kind)
	var path string
	var patch any
	switch adapter.action {
	case RolloutRestart:
		path = workloadPath(proposal.Target, resource, namespaced)
		patch = map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]string{"versus.dev/restartedAt": proposal.CreatedAt.UTC().Format(time.RFC3339Nano)}}}}}
	case Scale:
		path = workloadPath(proposal.Target, resource, namespaced) + "/scale"
		var params struct {
			Replicas int `json:"replicas"`
		}
		if err := json.Unmarshal(proposal.Params, &params); err != nil {
			return "", nil, err
		}
		patch = map[string]any{"spec": map[string]int{"replicas": params.Replicas}}
	case CronJobSuspend, CronJobResume:
		path = namespacedPath("batch", "v1", proposal.Target.Namespace, "cronjobs", proposal.Target.Name)
		patch = map[string]any{"spec": map[string]bool{"suspend": adapter.action == CronJobSuspend}}
	case NodeCordon, NodeUncordon:
		path = "/api/v1/nodes/" + proposal.Target.Name
		patch = map[string]any{"spec": map[string]bool{"unschedulable": adapter.action == NodeCordon}}
	default:
		return "", nil, errors.New("action type is not allow-listed")
	}
	encoded, err := json.Marshal(patch)
	return path, encoded, err
}

func (adapter Adapter) verifyOnce(ctx context.Context, proposal act.Proposal) (bool, error) {
	if adapter.action == CronJobTrigger {
		var binding cronJobBinding
		if err := json.Unmarshal(proposal.Params, &binding); err != nil || binding.SourceUID == "" || !proposalIDPattern.MatchString(proposal.ID) {
			return false, errors.New("invalid CronJob proposal binding")
		}
		data, err := adapter.api.Do(ctx, http.MethodGet, cronJobJobPath(proposal), nil, nil)
		if err != nil {
			return false, err
		}
		var job struct {
			Status struct {
				Active    int `json:"active"`
				Succeeded int `json:"succeeded"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &job); err != nil {
			return false, errors.New("invalid Kubernetes Job verification response")
		}
		return job.Status.Active > 0 || job.Status.Succeeded > 0, nil
	}
	resource, namespaced, ok := workloadResource(proposal.Target.Kind)
	if !ok {
		return false, errors.New("unsupported workload")
	}
	path := workloadPath(proposal.Target, resource, namespaced)
	if adapter.action == Scale {
		path = namespacedPath("apps", "v1", proposal.Target.Namespace, resource, proposal.Target.Name)
	}
	if adapter.action == CronJobSuspend || adapter.action == CronJobResume {
		path = namespacedPath("batch", "v1", proposal.Target.Namespace, "cronjobs", proposal.Target.Name)
	}
	if adapter.action == NodeCordon || adapter.action == NodeUncordon {
		path = "/api/v1/nodes/" + proposal.Target.Name
	}
	data, err := adapter.api.Do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return false, err
	}
	var state struct {
		Spec struct {
			Replicas      *int  `json:"replicas"`
			Suspend       *bool `json:"suspend"`
			Unschedulable *bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			UpdatedReplicas    int   `json:"updatedReplicas"`
			ReadyReplicas      int   `json:"readyReplicas"`
			DesiredScheduled   int   `json:"desiredNumberScheduled"`
			UpdatedScheduled   int   `json:"updatedNumberScheduled"`
			NumberReady        int   `json:"numberReady"`
		} `json:"status"`
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return false, errors.New("invalid Kubernetes verification response")
	}
	switch adapter.action {
	case RolloutRestart:
		desired := 1
		if state.Spec.Replicas != nil {
			desired = *state.Spec.Replicas
		}
		updated, ready := state.Status.UpdatedReplicas, state.Status.ReadyReplicas
		if proposal.Target.Kind == "DaemonSet" {
			desired = state.Status.DesiredScheduled
			updated, ready = state.Status.UpdatedScheduled, state.Status.NumberReady
		}
		return state.Status.ObservedGeneration >= state.Metadata.Generation && updated >= desired && ready >= desired, nil
	case Scale:
		var params struct {
			Replicas int `json:"replicas"`
		}
		if err := json.Unmarshal(proposal.Params, &params); err != nil {
			return false, err
		}
		return state.Spec.Replicas != nil && *state.Spec.Replicas == params.Replicas && state.Status.ReadyReplicas == params.Replicas, nil
	case CronJobSuspend, CronJobResume:
		return state.Spec.Suspend != nil && *state.Spec.Suspend == (adapter.action == CronJobSuspend), nil
	case NodeCordon, NodeUncordon:
		return state.Spec.Unschedulable != nil && *state.Spec.Unschedulable == (adapter.action == NodeCordon), nil
	default:
		return false, errors.New("action type is not allow-listed")
	}
}

func (adapter Adapter) effect(proposal act.Proposal) string {
	switch adapter.action {
	case RolloutRestart:
		return fmt.Sprintf("Restart %s/%s in namespace %s", proposal.Target.Kind, proposal.Target.Name, proposal.Target.Namespace)
	case Scale:
		var params struct {
			Replicas int `json:"replicas"`
		}
		_ = json.Unmarshal(proposal.Params, &params)
		return fmt.Sprintf("Set %s/%s replicas to %d", proposal.Target.Kind, proposal.Target.Name, params.Replicas)
	case CronJobSuspend:
		return "Suspend CronJob " + proposal.Target.Namespace + "/" + proposal.Target.Name
	case CronJobResume:
		return "Resume CronJob " + proposal.Target.Namespace + "/" + proposal.Target.Name
	case NodeCordon:
		return "Mark Node " + proposal.Target.Name + " unschedulable"
	case NodeUncordon:
		return "Mark Node " + proposal.Target.Name + " schedulable"
	case CronJobTrigger:
		return "Create one Job from CronJob " + proposal.Target.Namespace + "/" + proposal.Target.Name
	default:
		return ""
	}
}

type cronJobBinding struct {
	SourceUID       string `json:"source_uid"`
	ResourceVersion string `json:"resource_version"`
	TemplateHash    string `json:"template_hash"`
}

func (adapter Adapter) readCronJob(ctx context.Context, proposal act.Proposal) ([]byte, string, string, error) {
	data, err := adapter.api.Do(ctx, http.MethodGet, namespacedPath("batch", "v1", proposal.Target.Namespace, "cronjobs", proposal.Target.Name), nil, nil)
	if err != nil {
		return nil, "", "", err
	}
	var cronJob struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			JobTemplate struct {
				Spec json.RawMessage `json:"spec"`
			} `json:"jobTemplate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &cronJob); err != nil || cronJob.Metadata.UID == "" || cronJob.Metadata.ResourceVersion == "" || !json.Valid(cronJob.Spec.JobTemplate.Spec) {
		return nil, "", "", errors.New("invalid CronJob template response")
	}
	var spec any
	if err := json.Unmarshal(cronJob.Spec.JobTemplate.Spec, &spec); err != nil {
		return nil, "", "", errors.New("invalid CronJob job template")
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return nil, "", "", err
	}
	return canonical, cronJob.Metadata.UID, cronJob.Metadata.ResourceVersion, nil
}

func (adapter Adapter) jobManifest(ctx context.Context, proposal act.Proposal) ([]byte, error) {
	var binding cronJobBinding
	if !proposalIDPattern.MatchString(proposal.ID) {
		return nil, errors.New("invalid action proposal ID")
	}
	if err := decodeStrict(proposal.Params, &binding); err != nil {
		return nil, err
	}
	jobSpec, uid, resourceVersion, err := adapter.readCronJob(ctx, proposal)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(jobSpec)
	if uid != binding.SourceUID || resourceVersion != binding.ResourceVersion || hex.EncodeToString(hash[:]) != binding.TemplateHash {
		return nil, errors.New("CronJob changed after proposal; create a new proposal")
	}
	var spec any
	if err := json.Unmarshal(jobSpec, &spec); err != nil {
		return nil, errors.New("invalid CronJob job template")
	}
	manifest := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":            cronJobJobName(proposal),
			"namespace":       proposal.Target.Namespace,
			"annotations":     map[string]string{"versus.dev/agent-run": proposal.RunID},
			"ownerReferences": []map[string]any{{"apiVersion": "batch/v1", "kind": "CronJob", "name": proposal.Target.Name, "uid": uid, "controller": true}},
		},
		"spec": spec,
	}
	return json.Marshal(manifest)
}

func cronJobPath(proposal act.Proposal) string {
	return strings.TrimSuffix(namespacedPath("batch", "v1", proposal.Target.Namespace, "jobs", ""), "/")
}

func cronJobJobPath(proposal act.Proposal) string {
	return namespacedPath("batch", "v1", proposal.Target.Namespace, "jobs", cronJobJobName(proposal))
}

func cronJobJobName(proposal act.Proposal) string {
	if !proposalIDPattern.MatchString(proposal.ID) {
		return ""
	}
	id := strings.ReplaceAll(proposal.ID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "versus-" + id
}

func workloadResource(kind string) (string, bool, bool) {
	switch kind {
	case "Deployment":
		return "deployments", true, true
	case "StatefulSet":
		return "statefulsets", true, true
	case "DaemonSet":
		return "daemonsets", true, true
	default:
		return "", false, false
	}
}

func workloadPath(target act.TargetRef, resource string, namespaced bool) string {
	if namespaced {
		return namespacedPath("apps", "v1", target.Namespace, resource, target.Name)
	}
	return "/apis/apps/v1/" + resource + "/" + target.Name
}

func namespacedPath(group, version, namespace, resource, name string) string {
	return "/apis/" + group + "/" + version + "/namespaces/" + namespace + "/" + resource + "/" + name
}

func validName(value string) bool      { return resourceNamePattern.MatchString(value) }
func validNamespace(value string) bool { return namespacePattern.MatchString(value) }

func emptyObject(data []byte) bool {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(data, &object) == nil && object != nil && len(object) == 0
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid trailing JSON")
	}
	return nil
}

var _ act.Adapter = Adapter{}
