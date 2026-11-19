package suspect

import (
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Alarm is one firing alarm on the alarm line, with every resource it
// monitors.
type Alarm struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account string `json:"account"`
	Region  string `json:"region"`
	// ResourceARNs are the monitored resources as canonical ARNs, resolved
	// to the inventory's ARN where the inventory has the resource. Empty
	// means the resource is unknown; such an alarm never gets a suspect.
	ResourceARNs []string `json:"resource_arns"`
	// Start is the alarm's state transition; StartKnown is false when AWS
	// did not report one.
	Start      time.Time  `json:"start,omitzero"`
	StartKnown bool       `json:"start_known"`
	End        *time.Time `json:"end"`
	// Children are the firing child alarms of a composite alarm.
	Children []string `json:"children,omitempty"`
}

// Alarms groups alarm health signals into one alarm each. The collector
// writes one signal per monitored resource, all with the alarm ARN as Ref;
// a signal without a resource marks an alarm whose resource is unknown.
// Alarms are sorted by start, unknown starts last, then by ID.
func Alarms(items []model.Health, inventory []model.Resource) []Alarm {
	byID := map[string]*Alarm{}
	var order []string
	for _, item := range items {
		if item.Source != model.SourceAlarm || item.Ref == "" {
			continue
		}
		alarm, ok := byID[item.Ref]
		if !ok {
			alarm = &Alarm{ID: item.Ref, Name: alarmName(item.Ref), Account: item.Account, Region: item.Region, ResourceARNs: []string{}}
			byID[item.Ref] = alarm
			order = append(order, item.Ref)
		}
		if !item.Since.IsZero() && (alarm.Start.IsZero() || item.Since.Before(alarm.Start)) {
			alarm.Start, alarm.StartKnown = item.Since.UTC(), true
		}
		for _, child := range item.Children {
			if !contains(alarm.Children, child) {
				alarm.Children = append(alarm.Children, child)
			}
		}
		if item.Resource == "" {
			continue
		}
		arn := item.Resource
		if resource, found := Resolve(arn, inventory); found {
			arn = resource.ARN
		}
		if Key(arn) != "" && !contains(alarm.ResourceARNs, arn) {
			alarm.ResourceARNs = append(alarm.ResourceARNs, arn)
		}
	}
	out := make([]Alarm, 0, len(order))
	for _, id := range order {
		alarm := byID[id]
		sort.Strings(alarm.ResourceARNs)
		sort.Strings(alarm.Children)
		out = append(out, *alarm)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartKnown != out[j].StartKnown {
			return out[i].StartKnown
		}
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Anchor is the alarm as a suspect anchor: its resources, the scaling
// groups that contain them and their applications, searched one hour back
// from the alarm start in the alarm's Region and the global stream.
func (alarm Alarm) Anchor(inventory []model.Resource) Anchor {
	targets := map[string]Scope{}
	var narrowed []model.Resource
	for _, arn := range alarm.ResourceARNs {
		targets[arn] = ScopeResource
		if resource, found := Resolve(arn, inventory); found {
			narrowed = append(narrowed, resource)
		}
	}
	for arn, scope := range Related(inventory, narrowed) {
		if scope > targets[arn] {
			targets[arn] = scope
		}
	}
	var regions []string
	if alarm.Region != "" {
		regions = []string{alarm.Region}
	}
	start := time.Time{}
	if alarm.StartKnown {
		start = alarm.Start
	}
	return Anchor{Kind: AnchorAlarm, ID: alarm.ID, Account: alarm.Account, Regions: regions, Start: start,
		Lookback: AlarmLookback, Targets: targets}
}

// AlarmHistoryStarts is where the alarm line's history begins: the latest
// of the oldest kept health generations of the alarm units in scope. Before
// it, at least one unit has no history, so an alarm there cannot be shown.
// Zero means no unit has history.
func AlarmHistoryStarts(oldest []time.Time) time.Time {
	var starts time.Time
	for _, at := range oldest {
		if at.IsZero() {
			continue
		}
		if at.After(starts) {
			starts = at
		}
	}
	return starts.UTC()
}

func alarmName(arn string) string {
	if _, name, found := strings.Cut(arn, ":alarm:"); found {
		return name
	}
	return arn
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}
