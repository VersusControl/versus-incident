package snapshot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// feedCursor pins one listing: RecordedBefore hides rows stored after the
// first page, and the anchor resumes after the last item shown.
type feedCursor struct {
	Version        int       `json:"v"`
	RecordedBefore time.Time `json:"r"`
	Anchor         time.Time `json:"t"`
	ID             string    `json:"i"`
	// Since and Until are the listing's window; later pages answer it.
	Since time.Time `json:"s,omitzero"`
	Until time.Time `json:"u,omitzero"`
	// Services is the listing's service filter; later pages answer it.
	Services []string `json:"sv,omitempty"`
}

// maxCursorBytes bounds a decoded cursor: the window, the anchor and a
// full service filter fit well within it.
const maxCursorBytes = 8192

func encodeCursor(cursor feedCursor) string {
	cursor.Version = 1
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(text string) (feedCursor, error) {
	var cursor feedCursor
	data, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(data) > maxCursorBytes || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.RecordedBefore.IsZero() {
		return feedCursor{}, ErrBadFeedQuery
	}
	return cursor, nil
}

// groupKey identifies a burst or stack group; it is the group's ID, so
// expanding one needs no stored state.
type groupKey struct {
	Kind         string    `json:"k"`
	Account      string    `json:"a"`
	Region       string    `json:"r"`
	EventName    string    `json:"e,omitempty"`
	Actor        string    `json:"p,omitempty"`
	SessionClass string    `json:"s,omitempty"`
	ErrorCode    string    `json:"c,omitempty"`
	From         time.Time `json:"f"`
	To           time.Time `json:"u"`
	// Stack is the stack operation's event ID.
	Stack string `json:"g,omitempty"`
}

func encodeGroup(key groupKey) string {
	data, _ := json.Marshal(key)
	return key.Kind + "-" + base64.RawURLEncoding.EncodeToString(data)
}

func decodeGroup(id string) (groupKey, error) {
	kind, rest, ok := strings.Cut(id, "-")
	if !ok || (kind != ItemBurst && kind != ItemGroup) {
		return groupKey{}, ErrGroupNotFound
	}
	data, err := base64.RawURLEncoding.DecodeString(rest)
	var key groupKey
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &key) != nil || key.Kind != kind || !key.To.After(key.From) {
		return groupKey{}, ErrGroupNotFound
	}
	return key, nil
}

// eventName renders an event as service:Action.
func eventName(source, name string) string {
	if source == "" {
		return name
	}
	return strings.TrimSuffix(source, ".amazonaws.com") + ":" + name
}

// displayActor names a principal without exposing a person: the role or
// user name, an SSO permission set, or the actor class. Email-shaped names
// are masked.
func displayActor(class, principal, sessionClass string) string {
	principal = changes.MaskPrincipal(principal)
	name := principal
	if index := strings.LastIndex(principal, "/"); index >= 0 {
		name = principal[index+1:]
	}
	if strings.HasPrefix(name, "AWSReservedSSO_") {
		set := strings.TrimPrefix(name, "AWSReservedSSO_")
		if index := strings.LastIndex(set, "_"); index > 0 {
			set = set[:index]
		}
		return set + " (SSO)"
	}
	switch {
	case name != "":
		return name
	case class == string(model.ActorAWSService):
		return "AWS service"
	case class == string(model.ActorPipeline):
		return "Pipeline"
	case class == string(model.ActorPerson):
		return "Person"
	}
	return "Unknown actor"
}

// burstActor names the principal of a burst: an SSO permission set is
// shared by people, so the burst says it came via that set.
func burstActor(class, principal, sessionClass string) string {
	name := displayActor(class, principal, sessionClass)
	if sessionClass == changes.SessionSSO {
		return "people via " + strings.TrimSuffix(name, " (SSO)")
	}
	return name
}

func itemFromRow(row storage.AWSChange) FeedItem {
	item := FeedItem{Kind: ItemChange, ID: row.EventID, Time: row.EventTime, Account: row.Account, Region: row.Region, Service: row.Service,
		EventName: row.EventName, ActorClass: row.ActorClass, Actor: displayActor(row.ActorClass, row.Actor, row.SessionClass), Summary: row.Summary,
		Resources: []string{}, Fields: []FeedField{}, anchor: row.EventTime}
	if row.ResourceARN != "" {
		item.Resources = append(item.Resources, row.ResourceARN)
	}
	if row.ErrorCode != "" {
		code := row.ErrorCode
		item.ErrorCode = &code
	}
	return item
}

// fillItem completes a change item from its stored change.
func (feed *ChangeFeed) fillItem(ctx context.Context, item *FeedItem, change model.Change) {
	item.EventName = eventName(change.EventSource, change.EventName)
	if len(change.Resources) > 0 {
		item.Resources = append([]string(nil), change.Resources...)
	}
	if feed.fields != nil {
		item.Fields = changedFields(feed.fields.Fields(ctx, change), change)
		return
	}
	item.Fields = changedFields(newOnlyFields(change.Fields), change)
}

// identifierFields name the resource a change acts on, not a value it
// changed; the resource and the sentence already show them.
var identifierFields = map[string]bool{
	"service": true, "cluster": true, "function": true, "family": true, "instance": true, "group": true,
	"route_table": true, "target_group": true, "listener": true, "database": true, "stack": true, "table": true,
	"queue": true, "role": true, "policy": true, "alarm": true, "bucket": true, "rule": true, "event_bus": true,
	"change_set": true, "name": true,
}

// isIdentifierField reports a field that only identifies the resource: a
// known identifier name, an *_arn or *_id field, or a value that is one of
// the change's own resources.
func isIdentifierField(field FeedField, change model.Change) bool {
	if identifierFields[field.Field] || strings.HasSuffix(field.Field, "_arn") || strings.HasSuffix(field.Field, "_id") {
		return true
	}
	if field.After == nil {
		return false
	}
	for _, resource := range change.Resources {
		if *field.After == resource || model.ResourceID(resource) == *field.After {
			return true
		}
	}
	return false
}

// changedFields drops identifier fields from a change's field lines unless
// their value is known to have changed, so a line always names something
// that changed. Hidden fields stay: the fact that they changed is shown.
func changedFields(fields []FeedField, change model.Change) []FeedField {
	out := make([]FeedField, 0, len(fields))
	for _, field := range fields {
		if field.BeforeStatus != BeforeHidden && isIdentifierField(field, change) {
			known := field.BeforeStatus == BeforeKnown && field.Before != nil && field.After != nil && *field.Before != *field.After
			if !known {
				continue
			}
		}
		out = append(out, field)
	}
	return out
}

// newOnlyFields shows each field's new value; the before value needs a
// snapshot and is filled by a FieldResolver.
func newOnlyFields(fields map[string]string) []FeedField {
	out := make([]FeedField, 0, len(fields))
	for _, name := range sortedKeysOf(fields) {
		value := fields[name]
		out = append(out, FeedField{Field: name, After: &value, BeforeStatus: BeforeNewOnly})
	}
	return out
}

func failureRowKey(row storage.AWSChange) string {
	return row.Account + "\x00" + row.Region + "\x00" + row.EventName + "\x00" + row.Actor + "\x00" + row.SessionClass + "\x00" + row.ErrorCode
}

func failureCountKey(count storage.AWSChangeCount) string {
	return count.Account + "\x00" + count.Region + "\x00" + count.EventName + "\x00" + count.Actor + "\x00" + count.SessionClass + "\x00" + count.ErrorCode
}

// burstSpan is one chain of failed-attempt minutes.
type burstSpan struct {
	first, last time.Time
	events      int64
	sample      storage.AWSChangeCount
}

// formBursts chains each principal, error code and event's failure minutes
// whose start is within window of the previous one, and keeps chains of at
// least min events. Counts are per minute, so the chain rule works at
// minute resolution.
func formBursts(counts []storage.AWSChangeCount, window time.Duration, minimum int) map[string][]burstSpan {
	byKey := map[string][]storage.AWSChangeCount{}
	for _, count := range counts {
		if count.Kind == storage.AWSChangeCountFailure {
			key := failureCountKey(count)
			byKey[key] = append(byKey[key], count)
		}
	}
	out := map[string][]burstSpan{}
	for key, list := range byKey {
		sort.Slice(list, func(left, right int) bool { return list[left].Minute.Before(list[right].Minute) })
		var spans []burstSpan
		for _, count := range list {
			if last := len(spans) - 1; last >= 0 && count.Minute.Sub(spans[last].last) <= window {
				spans[last].last = count.Minute
				spans[last].events += count.Events
				continue
			}
			spans = append(spans, burstSpan{first: count.Minute, last: count.Minute, events: count.Events, sample: count})
		}
		for _, span := range spans {
			if span.events >= int64(minimum) {
				out[key] = append(out[key], span)
			}
		}
	}
	return out
}

// rowsAsCounts stands in per-minute failure counts from rows, when counts
// cannot be filtered (a resource filter).
func rowsAsCounts(rows []storage.AWSChange) []storage.AWSChangeCount {
	byKey := map[string]*storage.AWSChangeCount{}
	for _, row := range rows {
		if row.ErrorCode == "" {
			continue
		}
		minute := row.EventTime.Truncate(time.Minute)
		key := failureRowKey(row) + "\x00" + minute.Format(time.RFC3339)
		if byKey[key] == nil {
			byKey[key] = &storage.AWSChangeCount{Kind: storage.AWSChangeCountFailure, Account: row.Account, Region: row.Region, EventName: row.EventName,
				Actor: row.Actor, SessionClass: row.SessionClass, ErrorCode: row.ErrorCode, Minute: minute, Service: row.Service, ActorClass: row.ActorClass}
		}
		byKey[key].Events++
	}
	out := make([]storage.AWSChangeCount, 0, len(byKey))
	for _, key := range sortedKeysOf(byKey) {
		out = append(out, *byKey[key])
	}
	return out
}

func isStackOperation(row storage.AWSChange) bool {
	switch row.Template {
	case "cloudformation.create_stack", "cloudformation.update_stack", "cloudformation.delete_stack", "cloudformation.execute_change_set":
		return row.ErrorCode == "" && row.Fields["stack"] != ""
	}
	return false
}

// buildItems groups the window's rows and counts into list items, before
// any paging: bursts, stack groups, routine counts and single changes.
func (feed *ChangeFeed) buildItems(stored storage.AWSChangeFeed, counts []storage.AWSChangeCount, showRoutine bool, options FeedOptions) []FeedItem {
	rows := stored.Rows
	used := make(map[string]bool, len(rows))
	var items []FeedItem

	bursts := formBursts(counts, options.BurstWindow, options.BurstMin)
	for _, key := range sortedKeysOf(bursts) {
		for _, span := range bursts[key] {
			sample := span.sample
			end := span.last.Add(time.Minute)
			group := groupKey{Kind: ItemBurst, Account: sample.Account, Region: sample.Region, EventName: sample.EventName, Actor: sample.Actor,
				SessionClass: sample.SessionClass, ErrorCode: sample.ErrorCode, From: span.first, To: end}
			first, last := span.first, end
			var resources []string
			for _, row := range rows {
				if row.ErrorCode != "" && failureRowKey(row) == key && !row.EventTime.Before(span.first) && row.EventTime.Before(end) {
					used[row.EventID] = true
					if row.ResourceARN != "" && !containsString(resources, row.ResourceARN) && len(resources) < 10 {
						resources = append(resources, row.ResourceARN)
					}
				}
			}
			count, code := span.events, sample.ErrorCode
			lastSeen := last.Add(-time.Second)
			name := eventName(sample.EventSource, sample.EventName)
			if resources == nil {
				resources = []string{}
			}
			items = append(items, FeedItem{Kind: ItemBurst, ID: encodeGroup(group), Time: first, EndTime: &lastSeen, Account: sample.Account, Region: sample.Region,
				Service: sample.Service, EventName: name, ActorClass: sample.ActorClass, Actor: burstActor(sample.ActorClass, sample.Actor, sample.SessionClass),
				Summary: "Failed attempts: " + name + " (" + sample.ErrorCode + ")", Resources: resources, Fields: []FeedField{}, ErrorCode: &code, Count: &count, anchor: first})
		}
	}

	// Stack runs: a pipeline's stack operation and the calls CloudFormation
	// then made for the same principal become one group. A call joins the
	// newest operation before it.
	var operations []storage.AWSChange
	for _, row := range rows {
		if isStackOperation(row) {
			operations = append(operations, row)
		}
	}
	sort.Slice(operations, func(left, right int) bool { return operations[left].EventTime.Before(operations[right].EventTime) })
	members := map[string][]storage.AWSChange{}
	for _, row := range rows {
		if used[row.EventID] || row.Via != "cloudformation" || row.ErrorCode != "" {
			continue
		}
		var owner *storage.AWSChange
		for index := range operations {
			operation := &operations[index]
			if operation.Account == row.Account && operation.Region == row.Region && operation.Actor == row.Actor &&
				!row.EventTime.Before(operation.EventTime) && row.EventTime.Before(operation.EventTime.Add(stackGroupWindow)) && row.EventID != operation.EventID {
				owner = operation
			}
		}
		if owner != nil {
			members[owner.EventID] = append(members[owner.EventID], row)
		}
	}
	for _, operation := range operations {
		list := members[operation.EventID]
		if len(list) == 0 {
			continue
		}
		used[operation.EventID] = true
		newest := operation.EventTime
		for _, row := range list {
			used[row.EventID] = true
			if row.EventTime.After(newest) {
				newest = row.EventTime
			}
		}
		count := int64(len(list))
		group := groupKey{Kind: ItemGroup, Account: operation.Account, Region: operation.Region, Actor: operation.Actor, From: operation.EventTime,
			To: operation.EventTime.Add(stackGroupWindow), Stack: operation.EventID}
		item := itemFromRow(operation)
		item.Kind, item.ID, item.EndTime, item.Count = ItemGroup, encodeGroup(group), &newest, &count
		item.Summary = "Applied " + itoa(count) + " changes to stack " + operation.Fields["stack"]
		item.EventName = "cloudformation:" + operation.EventName
		items = append(items, item)
	}

	for _, row := range rows {
		if !used[row.EventID] {
			items = append(items, itemFromRow(row))
		}
	}

	if showRoutine {
		type routineKey struct{ account, region, name, source, service string }
		hours := map[routineKey]map[time.Time]int64{}
		for _, count := range counts {
			if count.Kind != storage.AWSChangeCountRoutine {
				continue
			}
			key := routineKey{count.Account, count.Region, count.EventName, count.EventSource, count.Service}
			if hours[key] == nil {
				hours[key] = map[time.Time]int64{}
			}
			hours[key][count.Minute.Truncate(time.Hour)] += count.Events
		}
		for key, byHour := range hours {
			for hour, events := range byHour {
				total := events
				end := hour.Add(time.Hour - time.Second)
				name := eventName(key.source, key.name)
				items = append(items, FeedItem{Kind: ItemRoutine, ID: "routine-" + base64.RawURLEncoding.EncodeToString([]byte(key.account+"|"+key.region+"|"+name+"|"+hour.Format(time.RFC3339))),
					Time: hour, EndTime: &end, Account: key.account, Region: key.region, Service: key.service, EventName: name, ActorClass: string(model.ActorAWSService),
					Actor: "AWS service", Summary: "Routine " + name + " events", Resources: []string{}, Fields: []FeedField{}, Count: &total, anchor: hour})
			}
		}
	}
	sortItems(items)
	return items
}

func itoa(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// sortItems orders the list newest first by anchor, then by ID.
func sortItems(items []FeedItem) {
	sort.Slice(items, func(left, right int) bool {
		if !items[left].anchor.Equal(items[right].anchor) {
			return items[left].anchor.After(items[right].anchor)
		}
		return items[left].ID > items[right].ID
	})
}

// afterCursor reports whether an item comes after the cursor's position.
func afterCursor(item FeedItem, cursor feedCursor) bool {
	if !item.anchor.Equal(cursor.Anchor) {
		return item.anchor.Before(cursor.Anchor)
	}
	return item.ID < cursor.ID
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return DefaultFeedLimit
	}
	return min(limit, MaxFeedLimit)
}

// page cuts one page and the next cursor.
func page(items []FeedItem, cursor feedCursor, hasCursor bool, limit int) ([]FeedItem, *string) {
	start := 0
	if hasCursor {
		start = len(items)
		for index, item := range items {
			if afterCursor(item, cursor) {
				start = index
				break
			}
		}
	}
	end := min(start+limit, len(items))
	out := append([]FeedItem(nil), items[start:end]...)
	if end >= len(items) || len(out) == 0 {
		return out, nil
	}
	last := out[len(out)-1]
	next := encodeCursor(feedCursor{RecordedBefore: cursor.RecordedBefore, Anchor: last.anchor, ID: last.ID, Since: cursor.Since, Until: cursor.Until, Services: cursor.Services})
	return out, &next
}
