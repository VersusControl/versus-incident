package kubernetes

import "testing"

// A cluster that does not serve the Metrics API says so on the usage and top
// answers, apart from a denied or a failed read of an API it does serve.
func TestMetricsAPIStateTellsNotInstalledFromDeniedAndFailed(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	usage, err := cluster.service.Usage(t.Context(), "", 5)
	if err != nil || usage.Metrics != "not_installed" {
		t.Fatalf("usage on a cluster without the Metrics API answered metrics=%q err=%v", usage.Metrics, err)
	}
	top, err := cluster.service.Top(t.Context(), TopOptions{Kind: "pod", Sort: "cpu", Limit: 5})
	if err != nil || top.Metrics != "not_installed" {
		t.Fatalf("top pod usage on a cluster without the Metrics API answered metrics=%q err=%v", top.Metrics, err)
	}
	for _, test := range []struct {
		name  string
		usage Usage
		want  string
	}{
		{"denied", Usage{Partial: []PartialFailure{{ResourceID: "metrics.k8s.io~v1beta1~pods", Class: "forbidden"}, {ResourceID: "metrics.k8s.io~v1beta1~nodes", Class: "forbidden"}}}, "forbidden"},
		{"failed", Usage{Partial: []PartialFailure{{ResourceID: "metrics.k8s.io~v1beta1~pods", Class: "timeout"}}}, "unavailable"},
		{"read", Usage{PodMetrics: MetricsSourceStatus{Total: 3}}, "available"},
	} {
		if got := metricsAPIState(test.usage, "unavailable"); got != test.want {
			t.Errorf("%s: metrics=%q, want %q", test.name, got, test.want)
		}
	}
}
