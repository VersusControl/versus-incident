package snapshot

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func (feed *ChangeFeed) feeds() (storage.AWSChangeFeedStore, error) {
	if feed == nil || !feed.store.ready() {
		return nil, ErrNoStorage
	}
	feeds, ok := feed.store.inventory.(storage.AWSChangeFeedStore)
	if !ok {
		return nil, ErrNoStorage
	}
	return feeds, nil
}

// validActorClass reports a known actor class filter.
func validActorClass(class string) bool {
	switch model.ActorClass(class) {
	case "", model.ActorPipeline, model.ActorPerson, model.ActorAWSService, model.ActorUnknown:
		return true
	}
	return false
}

// Answer is the change list for one query. It never claims "no changes"
// for a stretch it did not read: unread stretches are named gaps, the
// unsettled tail is reported apart, and a capped read is truncated and
// incomplete.
func (feed *ChangeFeed) Answer(ctx context.Context, request FeedRequest) (ChangesAnswer, error) {
	ctx, _ = feed.store.withInherited(ctx)
	feeds, err := feed.feeds()
	if err != nil {
		return ChangesAnswer{}, err
	}
	if request.Window <= 0 {
		request.Window = FeedWindowDay
	}
	if !validActorClass(request.ActorClass) || len(request.Text) > 200 || len(request.Resource) > 2048 {
		return ChangesAnswer{}, ErrBadFeedQuery
	}
	resourceKey := ""
	if request.Resource != "" {
		// The resource filter takes a canonical ARN only; a short name
		// would match unrelated resources.
		if resourceKey = changes.ResourceKey(request.Resource); resourceKey == "" {
			return ChangesAnswer{}, ErrBadFeedQuery
		}
	}
	now := feed.now().UTC()
	limits := feed.Limits()
	maxWindow := limits.MaxWindow
	cursor := feedCursor{RecordedBefore: now}
	hasCursor := request.Cursor != ""
	if hasCursor {
		if cursor, err = decodeCursor(request.Cursor); err != nil {
			return ChangesAnswer{}, err
		}
		// Later pages keep the first page's window, so a page read minutes
		// later never shifts or drops items. Cursors are not signed: the
		// tier is checked against the real now, with an hour's allowance
		// for paging, and an older window is expired.
		// A cursor without its window was not issued by this server.
		if cursor.Since.IsZero() || cursor.Until.IsZero() || cursor.Since.Before(now.Add(-maxWindow-CursorAllowance)) {
			return ChangesAnswer{}, ErrCursorExpired
		}
		request.Since, request.Until = cursor.Since, cursor.Until
		// The service filter, like the window, is the first page's.
		request.Services = cursor.Services
		if cursor.RecordedBefore.After(now) {
			cursor.RecordedBefore = now
		}
	}
	until := request.Until.UTC()
	if until.IsZero() || until.After(now) {
		until = now
	}
	if !request.Since.IsZero() {
		since := request.Since.UTC()
		if since.After(until) {
			return ChangesAnswer{}, ErrBadFeedQuery
		}
		request.Window = until.Sub(since)
	}
	if request.Window > maxWindow {
		return ChangesAnswer{}, ErrWindowLocked
	}
	since := until.Add(-request.Window)
	// Every request, cursor or not, is held to the tier against the real
	// now: a first page may start at most the tier window back, a later page at most
	// the paging allowance more.
	switch {
	case hasCursor && now.Sub(since) > maxWindow+CursorAllowance:
		return ChangesAnswer{}, ErrCursorExpired
	case !hasCursor && now.Sub(since) > maxWindow+time.Minute:
		return ChangesAnswer{}, ErrWindowLocked
	}
	if request.Services, err = NormalizeServices(request.Services); err != nil {
		return ChangesAnswer{}, err
	}
	cursor.Since, cursor.Until, cursor.Services = since, until, request.Services
	options := feed.effectiveOptions()
	query := storage.AWSChangeFeedQuery{
		Account: request.Account, Region: request.Region, Service: request.Service, Services: request.Services, ActorClass: request.ActorClass,
		ResourceARN: request.Resource, ResourceKey: resourceKey, Text: request.Text, Since: since, Until: until, RecordedBefore: cursor.RecordedBefore,
		RowLimit: options.ReadCap, ServiceCounts: request.ServiceFacet, Inherited: feed.store.inherited(ctx).limit(),
	}
	stored, err := feeds.AWSChangeFeed(ctx, feed.store.scope, query)
	if err != nil {
		return ChangesAnswer{}, err
	}
	pairs, accounts, err := feed.expectedPairs(ctx, request, stored)
	if err != nil {
		return ChangesAnswer{}, err
	}
	view := feed.coverage(pairs, accounts, stored, since, until, now, options)
	view.settleDisconnected(accounts, pairs)

	answer := ChangesAnswer{
		Window:   FeedWindow{Since: since, Until: until, SettledUntil: view.settled},
		Limits:   limits.View(),
		Gaps:     view.gaps,
		Accounts: view.accounts,
		Regions:  view.regions,
		Sync:     view.sync,
		Alarms:   []FeedAlarm{},
	}
	counts := stored.Counts
	var failureRows []storage.AWSChange
	if request.Resource != "" {
		// Counts carry no resource: errors come from the kept rows, so the
		// total is a lower bound.
		for _, row := range stored.Rows {
			if row.ErrorCode != "" {
				failureRows = append(failureRows, row)
			}
		}
		if failureRows == nil {
			failureRows = []storage.AWSChange{}
		}
		counts = rowsAsCounts(stored.Rows)
	}
	width := bucketWidth(request.Window)
	answer.BucketSeconds = int64(width / time.Second)
	answer.Buckets = buckets(since, until, width, stored, failureRows, view)
	answer.Summary = summarize(stored, counts, failureRows)
	answer.Summary.TotalIsLowerBound = !view.complete || stored.Truncated || request.Resource != "" && answer.Summary.Errors > 0
	answer.Summary.Truncated = stored.Truncated
	answer.Complete = view.complete && !stored.Truncated

	items := feed.buildItems(stored, counts, request.ShowRoutine, options)
	pageItems, next := page(items, cursor, hasCursor, pageLimit(request.Limit))
	if err := feed.fillItems(ctx, feeds, pageItems); err != nil {
		return ChangesAnswer{}, err
	}
	answer.Items, answer.NextCursor = pageItems, next
	if answer.Items == nil {
		answer.Items = []FeedItem{}
	}
	if request.ServiceFacet {
		answer.Services = serviceFacet(stored.ServiceCounts)
	}

	alarmsKnown := true
	if line, ok := feed.suspect.(AlarmLine); ok {
		alarms, starts, err := line.AlarmLine(ctx, AlarmScope{Account: request.Account, Region: request.Region, Since: since, Until: until})
		if err != nil {
			alarmsKnown = false
		} else {
			answer.Alarms, answer.AlarmHistoryStarts = alarms, starts
		}
	} else {
		answer.Alarms = feed.alarms(ctx, request, since, until)
	}
	if answer.Alarms == nil {
		answer.Alarms = []FeedAlarm{}
	}
	if !alarmsKnown {
		// The alarms could not be read: whether one fired is unknown, so
		// the answer is incomplete, never "no alarm".
		answer.Suspect = FeedSuspect{Status: SuspectIncomplete}
		markInFilter(&answer.Suspect, request)
		return answer, nil
	}
	suspectStored, suspectItems, suspectAnswer := stored, items, answer
	if len(request.Services) > 0 && len(answer.Alarms) > 0 {
		// An alarm's likely cause is never hidden by the service filter:
		// it is chosen from every service's changes in the window.
		query.Services, query.ServiceCounts = nil, false
		all, err := feeds.AWSChangeFeed(ctx, feed.store.scope, query)
		if err != nil {
			return ChangesAnswer{}, err
		}
		allCounts := all.Counts
		if request.Resource != "" {
			allCounts = rowsAsCounts(all.Rows)
		}
		suspectStored, suspectItems = all, feed.buildItems(all, allCounts, request.ShowRoutine, options)
		suspectAnswer.Complete = view.complete && !all.Truncated
	}
	answer.Suspect = feed.findSuspect(ctx, feeds, suspectStored, suspectAnswer, suspectItems, request, view)
	markInFilter(&answer.Suspect, request)
	return answer, nil
}

// markInFilter tells the page whether the suspect change is in a service
// its filter shows. A suspect with no change is never hidden.
func markInFilter(suspect *FeedSuspect, request FeedRequest) {
	if !request.ServiceFacet {
		return
	}
	in := suspect.Change == nil || serviceInFilter(request.Services, suspect.Change.Service)
	suspect.InFilter = &in
}

func summarize(stored storage.AWSChangeFeed, counts []storage.AWSChangeCount, failureRows []storage.AWSChange) FeedSummary {
	summary := FeedSummary{ByActorClass: map[string]int64{string(model.ActorPipeline): 0, string(model.ActorPerson): 0, string(model.ActorAWSService): 0, string(model.ActorUnknown): 0},
		People: stored.People, TopActors: []FeedActorCount{}}
	for _, minute := range stored.Minutes {
		class := minute.ActorClass
		if _, ok := summary.ByActorClass[class]; !ok {
			class = string(model.ActorUnknown)
		}
		summary.ByActorClass[class] += minute.Events
		summary.Total += minute.Events
	}
	for _, count := range counts {
		switch count.Kind {
		case storage.AWSChangeCountRoutine:
			summary.Routine += count.Events
		case storage.AWSChangeCountFailure:
			if failureRows == nil {
				summary.Errors += count.Events
			}
		}
	}
	if failureRows != nil {
		summary.Errors = int64(len(failureRows))
	}
	summary.TopActors = mergeTopActors(stored.TopActors)
	if !stored.LastBefore.IsZero() {
		at := stored.LastBefore
		summary.LastChangeBefore = &at
	}
	return summary
}

// maxTopActors is how many principals the answer line names.
const maxTopActors = 5

// mergeTopActors folds principals that read alike into one entry: the same
// role in two accounts or Regions is one actor and class with the summed
// count, so a label never repeats. Most first, then by label.
func mergeTopActors(stored []storage.AWSChangeActorCount) []FeedActorCount {
	out := []FeedActorCount{}
	index := map[string]int{}
	for _, actor := range stored {
		label := displayActor(actor.ActorClass, actor.Actor, actor.SessionClass)
		key := label + "\x00" + actor.ActorClass
		if at, ok := index[key]; ok {
			out[at].Count += actor.Events
			continue
		}
		index[key] = len(out)
		out = append(out, FeedActorCount{Actor: label, ActorClass: actor.ActorClass, Count: actor.Events})
	}
	sort.SliceStable(out, func(left, right int) bool {
		a, b := out[left], out[right]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Actor != b.Actor {
			return a.Actor < b.Actor
		}
		return a.ActorClass < b.ActorClass
	})
	if len(out) > maxTopActors {
		out = out[:maxTopActors]
	}
	return out
}

// fillItems loads the stored change behind each change item on the page,
// for its resources, fields and service:Action name.
func (feed *ChangeFeed) fillItems(ctx context.Context, feeds storage.AWSChangeFeedStore, items []FeedItem) error {
	var ids []string
	for _, item := range items {
		if item.Kind == ItemChange {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := feed.store.changesByID(ctx, feeds, ids)
	if err != nil {
		return err
	}
	byID := make(map[string]model.Change, len(rows))
	for _, row := range rows {
		if change, ok := changeOf(row); ok {
			byID[row.EventID] = change
		}
	}
	for index := range items {
		if change, ok := byID[items[index].ID]; ok && items[index].Kind == ItemChange {
			feed.fillItem(ctx, &items[index], change)
		}
	}
	return nil
}

// alarms lists the alarms firing in scope: one per alarm, with every
// resource it watches.
func (feed *ChangeFeed) alarms(ctx context.Context, request FeedRequest, since, until time.Time) []FeedAlarm {
	out := []FeedAlarm{}
	region := request.Region
	if region == model.GlobalRegion {
		return out
	}
	result, err := feed.store.Health(ctx, Query{Account: request.Account, Region: region})
	if err != nil {
		return out
	}
	byID := map[string]*FeedAlarm{}
	for _, health := range result.Health {
		if health.Source != model.SourceAlarm || health.Ref == "" {
			continue
		}
		if !health.Since.IsZero() && !health.Since.Before(until) {
			continue
		}
		alarm := byID[health.Ref]
		if alarm == nil {
			name := health.Ref
			if index := strings.LastIndex(name, ":"); index >= 0 {
				name = name[index+1:]
			}
			alarm = &FeedAlarm{ID: health.Ref, Name: name, Account: health.Account, Region: health.Region, ResourceARNs: []string{}}
			if !health.Since.IsZero() {
				start := health.Since
				alarm.Start, alarm.StartKnown = &start, true
			}
			byID[health.Ref] = alarm
		}
		if health.Resource != "" && !containsString(alarm.ResourceARNs, health.Resource) {
			alarm.ResourceARNs = append(alarm.ResourceARNs, health.Resource)
		}
	}
	for _, id := range sortedKeysOf(byID) {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(left, right int) bool {
		if out[left].Start == nil || out[right].Start == nil {
			return out[left].Start != nil
		}
		return out[left].Start.Before(*out[right].Start)
	})
	return out
}

// findSuspect asks the engine and attaches the full suspect item.
func (feed *ChangeFeed) findSuspect(ctx context.Context, feeds storage.AWSChangeFeedStore, stored storage.AWSChangeFeed, answer ChangesAnswer, items []FeedItem,
	query FeedRequest, view coverageView) FeedSuspect {
	request := SuspectRequest{Account: query.Account, Region: query.Region, Alarms: answer.Alarms, Gaps: answer.Gaps, SettledUntil: answer.Window.SettledUntil,
		Complete: answer.Complete, Since: answer.Window.Since, Until: answer.Window.Until, Settled: view.settledFunc()}
	for _, row := range stored.Rows {
		change := model.Change{ID: row.EventID, Time: row.EventTime, Account: row.Account, Region: row.Region, Service: row.Service, EventName: row.EventName,
			Template: row.Template, Summary: row.Summary, ErrorCode: row.ErrorCode, Fields: row.Fields,
			Actor: model.Actor{Class: model.ActorClass(row.ActorClass), PrincipalARN: row.Actor, SessionClass: row.SessionClass, Via: row.Via}}
		if row.ResourceARN != "" {
			change.Resources = []string{row.ResourceARN}
		}
		request.Changes = append(request.Changes, change)
	}
	request.Load = func(ctx context.Context, ids []string) ([]model.Change, error) {
		rows, err := feed.store.changesByID(ctx, feeds, ids)
		if err != nil {
			return nil, err
		}
		out := make([]model.Change, 0, len(rows))
		for _, row := range rows {
			if change, ok := changeOf(row); ok {
				out = append(out, change)
			}
		}
		return out, nil
	}
	suspect := feed.suspect.Suspect(ctx, request)
	if suspect.Status == "" {
		suspect.Status = SuspectNoAlarm
	}
	suspect.Change = nil
	if suspect.ChangeID == "" {
		return suspect
	}
	for _, item := range items {
		if item.ID == suspect.ChangeID {
			found := item
			if found.Kind == ItemChange {
				list := []FeedItem{found}
				if err := feed.fillItems(ctx, feeds, list); err == nil {
					found = list[0]
				}
			}
			suspect.Change = &found
			return suspect
		}
	}
	rows, err := feed.store.changesByID(ctx, feeds, []string{suspect.ChangeID})
	if err == nil && len(rows) == 1 {
		item := itemFromRow(rows[0])
		if change, ok := changeOf(rows[0]); ok {
			feed.fillItem(ctx, &item, change)
		}
		suspect.Change = &item
	}
	return suspect
}

// Group lists a burst's or stack group's member changes, newest first.
func (feed *ChangeFeed) Group(ctx context.Context, id, cursorText string, limit int) (GroupAnswer, error) {
	ctx, _ = feed.store.withInherited(ctx)
	feeds, err := feed.feeds()
	if err != nil {
		return GroupAnswer{}, err
	}
	key, err := decodeGroup(id)
	if err != nil {
		return GroupAnswer{}, err
	}
	// Group tokens are not signed either: the same tier check applies.
	if key.From.Before(feed.now().UTC().Add(-feed.Limits().MaxWindow - CursorAllowance)) {
		return GroupAnswer{}, ErrCursorExpired
	}
	cursor := feedCursor{RecordedBefore: feed.now().UTC()}
	hasCursor := cursorText != ""
	if hasCursor {
		if cursor, err = decodeCursor(cursorText); err != nil {
			return GroupAnswer{}, err
		}
	}
	stored, err := feeds.AWSChangeFeed(ctx, feed.store.scope, storage.AWSChangeFeedQuery{Account: key.Account, Region: key.Region, Since: key.From, Until: key.To,
		RecordedBefore: cursor.RecordedBefore, RowLimit: feed.effectiveOptions().ReadCap, Inherited: feed.store.inherited(ctx).limit()})
	if err != nil {
		return GroupAnswer{}, err
	}
	var items []FeedItem
	for _, row := range stored.Rows {
		switch key.Kind {
		case ItemBurst:
			if row.ErrorCode != key.ErrorCode || row.EventName != key.EventName || row.Actor != key.Actor || row.SessionClass != key.SessionClass {
				continue
			}
		case ItemGroup:
			if row.EventID != key.Stack && (row.Via != "cloudformation" || row.Actor != key.Actor || row.ErrorCode != "") {
				continue
			}
		}
		items = append(items, itemFromRow(row))
	}
	if key.Kind == ItemGroup && !containsItem(items, key.Stack) {
		return GroupAnswer{}, ErrGroupNotFound
	}
	sortItems(items)
	pageItems, next := page(items, cursor, hasCursor, pageLimit(limit))
	if err := feed.fillItems(ctx, feeds, pageItems); err != nil {
		return GroupAnswer{}, err
	}
	if pageItems == nil {
		pageItems = []FeedItem{}
	}
	return GroupAnswer{Items: pageItems, NextCursor: next}, nil
}

func containsItem(items []FeedItem, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
