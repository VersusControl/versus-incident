package breaklogs

import (
	"context"
	"regexp"
	"time"
)

// ReadError is a safe upstream class. It deliberately contains neither the
// upstream response body nor credential-bearing transport/configuration text.
type ReadError struct {
	Class string
}

func (err *ReadError) Error() string { return err.Class }

type Fields struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Time      string `json:"time,omitempty"`
	Message   string `json:"message,omitempty"`
}

var fieldPattern = regexp.MustCompile(`^[A-Za-z_@][A-Za-z0-9_.\-/@]{0,127}$`)
var splunkFieldPattern = regexp.MustCompile(`^[_A-Za-z][_A-Za-z0-9.]{0,63}$`)

func DefaultFields(kind string) Fields {
	switch kind {
	case "loki":
		return Fields{Namespace: "namespace", Pod: "pod", Container: "container"}
	case "elasticsearch":
		return Fields{Namespace: "kubernetes.namespace_name", Pod: "kubernetes.pod_name", Container: "kubernetes.container_name", Time: "@timestamp", Message: "message"}
	case "cloudwatchlogs":
		return Fields{Namespace: "kubernetes.namespace_name", Pod: "kubernetes.pod_name", Container: "kubernetes.container_name", Message: "log"}
	case "graylog":
		return Fields{Namespace: "kubernetes_namespace_name", Pod: "kubernetes_pod_name", Container: "kubernetes_container_name"}
	case "splunk":
		return Fields{Namespace: "namespace", Pod: "pod", Container: "container_name"}
	case "signoz":
		return Fields{Namespace: "k8s.namespace.name", Pod: "k8s.pod.name", Container: "k8s.container.name"}
	}
	return Fields{}
}

func NormalizeFields(kind string, fields Fields) (Fields, error) {
	defaults := DefaultFields(kind)
	if defaults.Namespace == "" {
		return Fields{}, ErrInvalid
	}
	if fields.Namespace == "" {
		fields.Namespace = defaults.Namespace
	}
	if fields.Pod == "" {
		fields.Pod = defaults.Pod
	}
	if fields.Container == "" {
		fields.Container = defaults.Container
	}
	if fields.Time == "" {
		fields.Time = defaults.Time
	}
	if fields.Message == "" {
		fields.Message = defaults.Message
	}
	pattern := fieldPattern
	if kind == "splunk" {
		pattern = splunkFieldPattern
	}
	for _, value := range []string{fields.Namespace, fields.Pod, fields.Container, fields.Time, fields.Message} {
		if value != "" && !pattern.MatchString(value) {
			return Fields{}, ErrInvalid
		}
	}
	if fields.Namespace == fields.Pod || fields.Namespace == fields.Container || fields.Pod == fields.Container {
		return Fields{}, ErrInvalid
	}
	return fields, nil
}

type LinkedQuery struct {
	Namespace string
	PodPrefix string
	Pods      []string
	Container string
	From, To  time.Time
	Limit     int
}

func (query LinkedQuery) Validate() error {
	if query.Namespace == "" || len(query.Namespace) > 253 || len(query.PodPrefix) > 253 || len(query.Container) > 63 || len(query.Pods) > 100 || query.From.IsZero() || !query.To.After(query.From) || query.To.Sub(query.From) > 2*time.Hour || query.Limit < 1 || query.Limit > 2000 {
		return ErrInvalid
	}
	return nil
}

type LinkedLine struct {
	At        time.Time `json:"at"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Text      string    `json:"text"`
}

type Result struct {
	Lines     []LinkedLine
	Truncated bool
	Complete  []Gap
	Rejected  bool
}

type LinkedReader interface {
	Read(context.Context, LinkedQuery) (Result, error)
	Preview(LinkedQuery) string
}

type Source struct {
	CacheKey  string                             `json:"-"`
	Name      string                             `json:"name"`
	Type      string                             `json:"type"`
	Scope     string                             `json:"scope"`
	Health    string                             `json:"health"`
	CheckedAt *time.Time                         `json:"checked_at,omitempty"`
	Build     func(Fields) (LinkedReader, error) `json:"-"`
}
