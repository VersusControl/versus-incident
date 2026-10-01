package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opsv1alpha1 "github.com/VersusControl/versus-incident/operator/api/v1alpha1"
)

const (
	dataPath   = "/app/data"
	configPath = "/app/config"
	appPort    = 3000
	runAsUser  = int64(65532)
)

// VersusIncidentReconciler reconciles a VersusIncident object into a
// ConfigMap, Deployment and Service.
type VersusIncidentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ops.versuscontrol.io,resources=versusincidents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ops.versuscontrol.io,resources=versusincidents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ops.versuscontrol.io,resources=versusincidents/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the cluster toward the desired state described by a
// VersusIncident resource.
func (r *VersusIncidentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var vi opsv1alpha1.VersusIncident
	if err := r.Get(ctx, req.NamespacedName, &vi); err != nil {
		// Not found: owner references handle child GC. Nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cfgYAML, sourcesYAML, err := buildConfigYAML(&vi)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render config: %w", err)
	}
	cfgHash := fmt.Sprintf("%x", sha256.Sum256([]byte(cfgYAML+sourcesYAML)))

	// 1) ConfigMap
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name(&vi), Namespace: vi.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels(&vi)
		cm.Data = map[string]string{"config.yaml": cfgYAML}
		if vi.Spec.Agent.Enable {
			cm.Data["agent_sources.yaml"] = sourcesYAML
		}
		return controllerutil.SetControllerReference(&vi, cm, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("configmap: %w", err)
	}

	// 2) Service
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name(&vi), Namespace: vi.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels(&vi)
		svc.Spec.Selector = selector(&vi)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       appPort,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(&vi, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("service: %w", err)
	}

	// 3) Deployment
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name(&vi), Namespace: vi.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		mutateDeployment(&vi, dep, cfgHash)
		return controllerutil.SetControllerReference(&vi, dep, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("deployment: %w", err)
	}

	// 4) Status
	if err := r.Get(ctx, types.NamespacedName{Name: name(&vi), Namespace: vi.Namespace}, dep); err == nil {
		base := vi.DeepCopy()
		vi.Status.ReadyReplicas = dep.Status.ReadyReplicas
		vi.Status.ObservedGeneration = vi.Generation
		setReady(&vi, dep.Status.ReadyReplicas > 0)
		// MergeFrom patch (no optimistic lock) avoids the benign "object has
		// been modified" conflict between the cached read and this write.
		if err := r.Status().Patch(ctx, &vi, client.MergeFrom(base)); err != nil {
			l.Error(err, "status patch failed")
		}
	}

	return ctrl.Result{}, nil
}

func mutateDeployment(vi *opsv1alpha1.VersusIncident, dep *appsv1.Deployment, cfgHash string) {
	replicas := int32(1)
	if vi.Spec.Replicas != nil {
		replicas = *vi.Spec.Replicas
	}
	img := fmt.Sprintf("%s:%s", orDefault(vi.Spec.Image.Repository, "ghcr.io/versuscontrol/versus-incident"), orDefault(vi.Spec.Image.Tag, "v1.4.3"))
	pull := vi.Spec.Image.PullPolicy
	if pull == "" {
		pull = corev1.PullIfNotPresent
	}

	dep.Labels = labels(vi)
	dep.Spec.Replicas = &replicas
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector(vi)}

	nonRoot := true
	fsGroup := runAsUser
	uid := runAsUser
	dep.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      selector(vi),
			Annotations: map[string]string{"versuscontrol.io/config-hash": cfgHash},
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup:      &fsGroup,
				RunAsNonRoot: &nonRoot,
				RunAsUser:    &uid,
			},
			Containers: []corev1.Container{{
				Name:            "versus-incident",
				Image:           img,
				ImagePullPolicy: pull,
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: appPort, Protocol: corev1.ProtocolTCP}},
				Env:             buildEnv(vi),
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					RunAsNonRoot:             &nonRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				LivenessProbe:  httpProbe(30),
				ReadinessProbe: httpProbe(5),
				VolumeMounts:   volumeMounts(vi),
			}},
			Volumes: volumes(vi),
		},
	}
}

func buildEnv(vi *opsv1alpha1.VersusIncident) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "STORAGE_TYPE", Value: "file"}}
	if vi.Spec.GatewaySecretName != "" {
		env = append(env, secretEnv("GATEWAY_SECRET", vi.Spec.GatewaySecretName, "gateway_secret"))
	}
	if vi.Spec.Telegram.Enabled {
		env = append(env,
			corev1.EnvVar{Name: "TELEGRAM_ENABLE", Value: "true"},
			secretEnv("TELEGRAM_BOT_TOKEN", vi.Spec.Telegram.SecretName, "telegram_bot_token"),
			secretEnv("TELEGRAM_CHAT_ID", vi.Spec.Telegram.SecretName, "telegram_chat_id"),
		)
	}
	if vi.Spec.Agent.Enable {
		env = append(env,
			corev1.EnvVar{Name: "AGENT_ENABLE", Value: "true"},
			corev1.EnvVar{Name: "AGENT_MODE", Value: orDefault(vi.Spec.Agent.Mode, "training")},
		)
		if vi.Spec.Agent.AI.Enable {
			env = append(env,
				corev1.EnvVar{Name: "AGENT_AI_ENABLE", Value: "true"},
				corev1.EnvVar{Name: "AGENT_AI_MODEL", Value: orDefault(vi.Spec.Agent.AI.Model, "gpt-4o-mini")},
				secretEnv("AGENT_AI_API_KEY", vi.Spec.Agent.AI.APIKeySecretName, "agent_ai_api_key"),
			)
		}
	}
	return env
}

func volumeMounts(vi *opsv1alpha1.VersusIncident) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: configPath + "/config.yaml", SubPath: "config.yaml"},
		{Name: "data", MountPath: dataPath},
	}
	if vi.Spec.Agent.Enable {
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: configPath + "/agent_sources.yaml", SubPath: "agent_sources.yaml"})
	}
	return mounts
}

func volumes(vi *opsv1alpha1.VersusIncident) []corev1.Volume {
	return []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name(vi)}}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}

func httpProbe(delay int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthz",
			Port: intstr.FromString("http"),
		}},
		InitialDelaySeconds: delay,
		PeriodSeconds:       10,
	}
}

func secretEnv(envName, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: envName, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
		Key:                  key,
	}}}
}

func setReady(vi *opsv1alpha1.VersusIncident, ready bool) {
	cond := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: vi.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if ready {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "DeploymentReady"
		cond.Message = "managed Deployment has ready replicas"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "DeploymentNotReady"
		cond.Message = "waiting for the managed Deployment to become ready"
	}
	// Replace any existing Ready condition.
	out := cond
	conds := []metav1.Condition{out}
	for _, c := range vi.Status.Conditions {
		if c.Type != "Ready" {
			conds = append(conds, c)
		}
	}
	vi.Status.Conditions = conds
}

func name(vi *opsv1alpha1.VersusIncident) string { return vi.Name }

func labels(vi *opsv1alpha1.VersusIncident) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "versus-incident",
		"app.kubernetes.io/instance":   vi.Name,
		"app.kubernetes.io/managed-by": "versus-incident-operator",
	}
}

func selector(vi *opsv1alpha1.VersusIncident) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "versus-incident",
		"app.kubernetes.io/instance": vi.Name,
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func ptr[T any](v T) *T { return &v }

// configTemplate renders the in-pod config.yaml. The agent detection blocks
// (regex / redaction / miner / catalog / service_patterns) are always
// included when the agent is enabled — without them the regex pre-filter is
// empty and the agent matches nothing.
var configTemplate = template.Must(template.New("config").Parse(`name: {{ .Name }}
host: 0.0.0.0
port: 3000

gateway_secret: ${GATEWAY_SECRET}

storage:
  type: file
  file:
    max_incidents: 1000

alert:
  debug_body: true
  telegram:
    enable: {{ .Telegram.Enabled }}
    bot_token: ${TELEGRAM_BOT_TOKEN}
    chat_id: ${TELEGRAM_CHAT_ID}
    template_path: "/app/config/telegram_message.tmpl"

redis:
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
{{- if .Agent.Enable }}

agent:
  enable: true
  mode: {{ .Agent.Mode }}
  poll_interval: {{ .Agent.PollInterval }}
  lookback: 10m
  new_service_grace: "0"
  sources_path: /app/config/agent_sources.yaml
  batch_max: 5000
  signal_max_bytes: 65536
  redaction:
    enable: true
    redact_ips: false
    extra_patterns:
      - "(?i)password=\\S+"
      - "Authorization:\\s*Bearer\\s+\\S+"
  miner:
    similarity_threshold: 0.4
    tree_depth: 4
    max_children: 100
  catalog:
    persist_interval: 30s
    auto_promote_after: 50
    spike_multiplier: 5.0
    spike_min_frequency: 5
    spike_min_baseline_count: 20
  regex:
    default_pattern: "(?i).*error.*"
    rules:
      - name: oom-killer
        pattern: "Out of memory: Killed process"
      - name: panic
        pattern: "(?i)panic:"
      - name: 5xx-burst
        pattern: "HTTP/[0-9.]+\\s+5\\d\\d"
  service_patterns:
    - '(?i)\bservice[._-]?name["\s:=]+"?([A-Za-z0-9._-]+)'
    - '(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)'
    - '\[([A-Za-z0-9._-]+)\]'
  ai:
    enable: {{ .Agent.AI.Enable }}
    api_key: ${AGENT_AI_API_KEY}
{{- if .Agent.AI.Provider }}
    provider: "{{ .Agent.AI.Provider }}"
{{- end }}
    model: "{{ .Agent.AI.Model }}"
    temperature: 0.2
    max_tokens: 1024
    max_calls_per_hour: 60
    cache_ttl: "1h"
{{- end }}
`))

var sourcesTemplate = template.Must(template.New("sources").Parse(`sources:
{{- if .Agent.Sources }}
{{- range .Agent.Sources }}
  - name: {{ .Name }}
    type: {{ .Type }}
    enable: {{ .Enable }}
{{- if and (eq .Type "file") .File }}
    file:
      path: {{ .File.Path }}
      from_beginning: {{ .File.FromBeginning }}
{{- if .File.Format }}
      format: {{ .File.Format }}
{{- end }}
{{- end }}
{{- if and (eq .Type "loki") .Loki }}
    loki:
      address: {{ .Loki.Address }}
      query: {{ .Loki.Query | printf "%q" }}
{{- if .Loki.TenantID }}
      tenant_id: {{ .Loki.TenantID }}
{{- end }}
{{- end }}
{{- if and (eq .Type "elasticsearch") .Elasticsearch }}
    elasticsearch:
      addresses:
{{- range .Elasticsearch.Addresses }}
        - {{ . }}
{{- end }}
      index: {{ .Elasticsearch.Index }}
{{- if .Elasticsearch.Query }}
      query: {{ .Elasticsearch.Query | printf "%q" }}
{{- end }}
{{- end }}
{{- end }}
{{- else }}
  []
{{- end }}
`))

// tmplData carries the CR name alongside the embedded spec so templates can
// reference both {{ .Name }} and the promoted spec fields ({{ .Agent... }}).
type tmplData struct {
	Name string
	opsv1alpha1.VersusIncidentSpec
}

func buildConfigYAML(vi *opsv1alpha1.VersusIncident) (string, string, error) {
	data := tmplData{Name: vi.Name, VersusIncidentSpec: vi.Spec}
	var cfg, src strings.Builder
	if err := configTemplate.Execute(&cfg, data); err != nil {
		return "", "", fmt.Errorf("config template: %w", err)
	}
	if err := sourcesTemplate.Execute(&src, data); err != nil {
		return "", "", fmt.Errorf("sources template: %w", err)
	}
	return cfg.String(), src.String(), nil
}

// SetupWithManager registers the controller and the resources it owns.
func (r *VersusIncidentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&opsv1alpha1.VersusIncident{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
