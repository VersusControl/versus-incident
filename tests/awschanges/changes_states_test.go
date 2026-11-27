package awschanges

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestChangesFixtureStates boots the real server on each state scenario of
// tests/fakeaws and checks over HTTP that the server reaches the state the
// scenario exists for: a disconnected account, a Region that refuses every
// call, late CloudTrail delivery, malformed events, hidden values, routine
// counts, e-mail-shaped principals, tied suspects and a compare with
// nothing different. Run one subtest per command to stay inside the time
// box (AWS_CHANGES_RUN='^TestChangesFixtureStates$/<name>').
func TestChangesFixtureStates(t *testing.T) {
	base := setup(t)
	prodOnly := map[string][]string{prodAccount: {"us-east-1"}}
	// Each subtest runs in its own command, so each keeps its own verdict.
	stateEnv := func(t *testing.T) *env {
		e := *base
		name := regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(t.Name(), "_")
		e.out = &results{path: filepath.Join(base.root, "changes-verdict-"+name+".json"), data: map[string]any{"storage": base.storage}}
		return &e
	}

	t.Run("account_disconnected_as_of", func(t *testing.T) {
		e := stateEnv(t)
		r, w := e.boot(t, "disconnected", fakeaws.ScenarioBadDeploy, options{changesInterval: "30s"},
			map[string][]string{prodAccount: {"us-east-1"}, stageAccount: {"us-east-1"}})
		before := r.waitPolled(150 * time.Second)
		stagedBefore := 0
		for _, item := range list(before, "items") {
			if text(item, "account") == stageAccount {
				stagedBefore++
			}
		}
		switched := time.Now().UTC()
		w.set(fakeaws.ScenarioRoleSessionExpired)
		var rechecks []any
		for range 2 {
			got := r.call(http.MethodPost, "/api/admin/aws/accounts/"+stageAccount+"/recheck", "")
			rechecks = append(rechecks, map[string]any{"status": got.status, "account_status": got.json()["status"], "error_code": got.json()["error_code"]})
		}
		settings := r.call(http.MethodGet, "/api/admin/aws/settings", "").json()
		accounts := map[string]any{}
		for _, account := range list(settings, "accounts") {
			accounts[text(account, "account_id")] = map[string]any{"status": path(account, "status"), "disconnected_since": path(account, "disconnected_since"), "error_code": path(account, "error_code")}
		}
		var view map[string]any
		var stage map[string]any
		poll(90*time.Second, func() bool {
			view = r.changes("?window=24h&limit=200")
			stage = nil
			for _, account := range list(view, "accounts") {
				if text(account, "account") == stageAccount {
					stage, _ = account.(map[string]any)
				}
			}
			return stage != nil && stage["as_of"] != nil
		})
		stagedAfter := 0
		for _, item := range list(view, "items") {
			if text(item, "account") == stageAccount {
				stagedAfter++
			}
		}
		e.out.put("account_disconnected", map[string]any{"switched": switched, "rechecks": rechecks, "settings_accounts": accounts,
			"changes_accounts": view["accounts"], "gap_reasons": gapReasons(view), "staging_items_before": stagedBefore, "staging_items_after": stagedAfter})
		if text(accounts[stageAccount], "status") != "disconnected" {
			t.Fatalf("Settings staging = %v after two rechecks, want disconnected", accounts[stageAccount])
		}
		if text(accounts[prodAccount], "status") != "connected" {
			t.Errorf("Settings production = %v, want connected", accounts[prodAccount])
		}
		if stage == nil || stage["complete"] != false || (text(stage, "reason") != "assume_role_failed" && text(stage, "reason") != "account_disconnected") {
			t.Errorf("changes staging = %v, want incomplete for the lost role", stage)
		}
		if stage != nil && stage["as_of"] == nil {
			t.Errorf("staging is disconnected (reason %v) but the changes answer gives no as_of", stage["reason"])
		}
		if stagedBefore == 0 || stagedAfter < stagedBefore {
			t.Errorf("staging changes before the disconnect: %d before, %d after", stagedBefore, stagedAfter)
		}
	})

	t.Run("region_not_enabled", func(t *testing.T) {
		e := stateEnv(t)
		// A config account lists its Regions in the order written, so its
		// home Region (where STS is signed) is the enabled us-east-1.
		r, _ := e.boot(t, "notenabled", fakeaws.ScenarioRegionNotEnabled, options{changesInterval: "30s", accounts: configAccount(prodAccount, "us-east-1", fakeaws.NotEnabledRegion)}, nil)
		var view map[string]any
		poll(150*time.Second, func() bool {
			// The claim holds once Settings has recorded the Region off, so
			// read the changes answer only after that.
			if !settingsNotEnabled(r, prodAccount, fakeaws.NotEnabledRegion) {
				return false
			}
			view = r.changes("?window=24h")
			// Every pair has recorded a poll, not only the refused one.
			return gapReasons(view)["region_not_enabled"] > 0 && pairsPolled(view)
		})
		// What the wizard's region discovery says about the same Region.
		created := r.call(http.MethodPost, "/api/admin/aws/connect/drafts", `{"method":"cloudformation"}`)
		draft, _ := created.json()["draft_id"].(string)
		r.call(http.MethodPost, "/api/admin/aws/connect/drafts/"+draft+"/verify", `{"role_arn":"arn:aws:iam::777788889999:role/versus-reader"}`)
		poll(60*time.Second, func() bool {
			status, _ := r.call(http.MethodGet, "/api/admin/aws/connect/drafts/"+draft, "").json()["status"].(string)
			return status == "verified"
		})
		discovered := r.call(http.MethodPost, "/api/admin/aws/connect/drafts/"+draft+"/regions", "")
		settings := r.call(http.MethodGet, "/api/admin/aws/settings", "").json()
		var prodRegions any
		for _, account := range list(settings, "accounts") {
			if text(account, "account_id") == prodAccount {
				prodRegions = path(account, "regions")
			}
		}
		var regionGaps []any
		for _, gap := range list(view, "gaps") {
			if text(gap, "region") == fakeaws.NotEnabledRegion {
				regionGaps = append(regionGaps, map[string]any{"reason": path(gap, "reason"), "from": path(gap, "from"), "to": path(gap, "to")})
			}
		}
		e.out.put("region_not_enabled", map[string]any{"complete": view["complete"], "regions": view["regions"], "accounts": view["accounts"],
			"region_gaps": regionGaps, "settings_regions": prodRegions, "wizard_discovery_status": discovered.status, "wizard_discovery": firstChars(string(discovered.body), 900)})
		notEnabledRegionChecks(t, view, regionGaps)
		if len(regionGaps) == 0 {
			t.Fatalf("no gap for %s: %v", fakeaws.NotEnabledRegion, view["gaps"])
		}
		for _, gap := range regionGaps {
			if text(gap, "reason") != "region_not_enabled" {
				t.Errorf("%s gap reason %v, want region_not_enabled", fakeaws.NotEnabledRegion, path(gap, "reason"))
			}
		}
		for _, gap := range list(view, "gaps") {
			if text(gap, "region") == "us-east-1" && text(gap, "reason") != "arriving" {
				t.Errorf("the enabled Region has a gap: %v", gap)
			}
		}
		if !strings.Contains(string(discovered.body), fakeaws.NotEnabledRegion) {
			t.Errorf("wizard discovery does not list %s: %.300s", fakeaws.NotEnabledRegion, discovered.body)
		}
	})

	t.Run("region_not_enabled_sorts_first", func(t *testing.T) {
		e := stateEnv(t)
		// A wizard account stores its Regions sorted; af-south-1 sorts
		// before us-east-1 and becomes the home Region.
		r, _ := e.boot(t, "notenabledfirst", fakeaws.ScenarioRegionNotEnabled, options{changesInterval: "30s"},
			map[string][]string{prodAccount: {"us-east-1", fakeaws.NotEnabledRegion}})
		poll(120*time.Second, func() bool {
			return text(r.changes("?window=24h"), "sync", "last_poll_at") != ""
		})
		// One more poll cycle, so both Regions have been polled again.
		r.waitPolls(1, 30*time.Second)
		view := r.changes("?window=24h")
		byRegion := map[string]map[string]int{}
		for _, gap := range list(view, "gaps") {
			region := text(gap, "region")
			if byRegion[region] == nil {
				byRegion[region] = map[string]int{}
			}
			byRegion[region][text(gap, "reason")]++
		}
		settings := r.call(http.MethodGet, "/api/admin/aws/settings", "").json()
		var account any
		for _, item := range list(settings, "accounts") {
			if text(item, "account_id") == prodAccount {
				account = map[string]any{"status": path(item, "status"), "error_code": path(item, "error_code"), "regions": path(item, "regions")}
			}
		}
		e.out.put("region_not_enabled_sorts_first", map[string]any{"gap_reasons_by_region": byRegion, "accounts": view["accounts"], "regions": view["regions"],
			"complete": view["complete"], "settings_account": account})
		if usEast := byRegion["us-east-1"]; len(usEast) > 0 && usEast["arriving"] != len(usEast) {
			t.Errorf("a not-enabled Region that sorts first takes the enabled us-east-1 down with it: gaps by Region %v", byRegion)
		}
		if text(account, "status") != "connected" {
			t.Errorf("Settings account = %v, want connected", account)
		}
		var regionGaps []any
		for _, gap := range list(view, "gaps") {
			if text(gap, "region") == fakeaws.NotEnabledRegion {
				regionGaps = append(regionGaps, gap)
			}
		}
		if len(regionGaps) == 0 {
			t.Errorf("no gap for %s: %v", fakeaws.NotEnabledRegion, view["gaps"])
		}
		notEnabledRegionChecks(t, view, regionGaps)
	})

	t.Run("region_not_enabled_unrecorded", func(t *testing.T) {
		e := stateEnv(t)
		// Settings never learns the Region is off (DescribeRegions does not
		// list it): its refusals come only from the polls, so they are a
		// real gap.
		r, _ := e.boot(t, "unrecorded", fakeaws.ScenarioRegionOptInUnrecorded, options{changesInterval: "30s", accounts: configAccount(prodAccount, "us-east-1", fakeaws.NotEnabledRegion)}, nil)
		var view map[string]any
		poll(150*time.Second, func() bool {
			view = r.changes("?window=24h")
			// Every pair has recorded a poll, not only the refused one.
			return gapReasons(view)["region_not_enabled"] > 0 && pairsPolled(view)
		})
		settings := r.call(http.MethodGet, "/api/admin/aws/settings", "").json()
		var settingsAccount any
		for _, item := range list(settings, "accounts") {
			if text(item, "account_id") == prodAccount {
				settingsAccount = map[string]any{"status": path(item, "status"), "regions": path(item, "regions")}
			}
		}
		state := ""
		for _, region := range list(view, "regions") {
			if text(region, "region") == fakeaws.NotEnabledRegion {
				state = text(region, "state")
			}
		}
		e.out.put("region_not_enabled_unrecorded", map[string]any{"complete": view["complete"], "lower_bound": path(view, "summary", "total_is_lower_bound"),
			"accounts": view["accounts"], "regions": view["regions"], "gap_reasons": gapReasons(view), "settings_account": settingsAccount})
		if gapReasons(view)["region_not_enabled"] == 0 {
			t.Fatalf("no region_not_enabled gap: %v", view["gaps"])
		}
		if notEnabled := list(settingsAccount, "regions", "not_enabled"); len(notEnabled) != 0 {
			t.Fatalf("Settings recorded the Region (%v): the run does not test an unrecorded Region", notEnabled)
		}
		if truth(view, "complete") || !truth(view, "summary", "total_is_lower_bound") {
			t.Errorf("poll-derived region_not_enabled: complete %v, lower bound %v; want incomplete and a lower bound", view["complete"], path(view, "summary", "total_is_lower_bound"))
		}
		for _, account := range list(view, "accounts") {
			if truth(account, "complete") {
				t.Errorf("account %v complete over an unread Region", account)
			}
		}
		if state == "not_enabled" {
			t.Errorf("an unrecorded Region is listed as confirmed not enabled")
		}
		for _, gap := range list(view, "gaps") {
			if text(gap, "region") == "us-east-1" && text(gap, "reason") != "arriving" {
				t.Errorf("the enabled Region has a gap: %v", gap)
			}
		}
	})

	t.Run("cursor_expired", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "cursor", fakeaws.ScenarioChangesEmailPrincipal, options{}, prodOnly)
		first := r.waitPolled(150 * time.Second)
		page := r.changes("?window=24h&limit=1")
		results := map[string]any{}
		// Live tokens still page.
		if cursor := text(page, "next_cursor"); cursor != "" {
			got := r.call(http.MethodGet, "/api/admin/aws/changes?cursor="+url.QueryEscape(cursor), "")
			results["live_cursor"] = got.status
			if got.status != http.StatusOK {
				t.Errorf("live cursor = %d %.200s", got.status, got.body)
			}
		} else {
			t.Errorf("no next_cursor on a one-item page: %v", page["items"])
		}
		for _, item := range itemsOf(first, "burst") {
			got := r.call(http.MethodGet, "/api/admin/aws/changes/groups/"+url.PathEscape(text(item, "id"))+"?limit=5", "")
			results["live_group"] = got.status
			if got.status != http.StatusOK {
				t.Errorf("live group = %d %.200s", got.status, got.body)
			}
		}
		// Tokens whose window starts 26 h and 7 days ago.
		for _, age := range []time.Duration{26 * time.Hour, 7 * 24 * time.Hour} {
			old := time.Now().UTC().Add(-age).Truncate(time.Second)
			cursorJSON, _ := json.Marshal(map[string]any{"v": 1, "r": old.Add(time.Hour), "t": old.Add(30 * time.Minute), "i": "~", "s": old, "u": old.Add(time.Hour)})
			groupJSON, _ := json.Marshal(map[string]any{"k": "burst", "a": prodAccount, "r": "us-east-1", "e": "ssm:PutParameter", "c": "AccessDenied", "f": old, "u": old.Add(time.Hour)})
			for name, target := range map[string]string{
				"cursor": "/api/admin/aws/changes?cursor=" + base64.RawURLEncoding.EncodeToString(cursorJSON),
				"group":  "/api/admin/aws/changes/groups/burst-" + base64.RawURLEncoding.EncodeToString(groupJSON),
			} {
				got := r.call(http.MethodGet, target, "")
				key := name + "_" + age.String()
				results[key] = map[string]any{"status": got.status, "body": firstChars(string(got.body), 120)}
				if got.status != http.StatusBadRequest || got.json()["error"] != "cursor_expired" {
					t.Errorf("%s from %s ago = %d %.200s, want 400 cursor_expired", name, age, got.status, got.body)
				}
			}
		}
		e.out.put("cursor_expired", results)
	})

	t.Run("late_arrival_counted", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "late", fakeaws.ScenarioChangesLateArrival, options{changesInterval: "30s"}, prodOnly)
		s := r.sample(2 * time.Second)
		first := r.waitPolled(150 * time.Second)
		firstLate := number(first, "sync", "late_arrivals")
		var view map[string]any
		poll(240*time.Second, func() bool {
			view = r.changes("?window=24h&limit=1")
			return number(view, "sync", "late_arrivals") > 0
		})
		samples, problems := s.finish()
		e.out.put("late_arrival", map[string]any{"late_after_first_read": firstLate, "late_now": number(view, "sync", "late_arrivals"),
			"sync": view["sync"], "complete": view["complete"], "total": number(view, "summary", "total"), "samples": samples, "violations": problems})
		if firstLate != 0 {
			t.Errorf("first read counted %d late arrivals; there was no earlier coverage", firstLate)
		}
		if number(view, "sync", "late_arrivals") == 0 {
			t.Errorf("no late arrival counted within the %s hang guard of 30 s polls: %v", scaledTimeout(240*time.Second), view["sync"])
		}
		if len(problems) > 0 {
			t.Errorf("invariant violations over %d samples: %v", samples, problems)
		}
	})

	t.Run("malformed_events_dropped_and_counted", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "malformed", fakeaws.ScenarioChangesMalformed, options{changesInterval: "30s"}, prodOnly)
		first := r.waitPolled(150 * time.Second)
		// Two more polls: nothing malformed is re-read, so the count holds.
		view := r.waitPolls(2, 30*time.Second)
		names := []string{}
		for _, item := range list(view, "items") {
			names = append(names, text(item, "event_name")+"/"+text(item, "actor_class"))
		}
		e.out.put("malformed", map[string]any{"dropped_first": number(first, "sync", "dropped"), "dropped_later": number(view, "sync", "dropped"),
			"complete": view["complete"], "total": number(view, "summary", "total"), "items": names, "gaps": view["gaps"]})
		if got := number(first, "sync", "dropped"); got != fakeaws.ChangesMalformedCount {
			// The us-east-1 pair and the global stream read the same
			// events; a malformed one is counted by both.
			t.Errorf("dropped after the first read = %d, want %d", got, fakeaws.ChangesMalformedCount)
		}
		if got := number(view, "sync", "dropped"); got != fakeaws.ChangesMalformedCount {
			t.Errorf("dropped after three polls = %d, want %d (nothing malformed was re-read)", got, fakeaws.ChangesMalformedCount)
		}
		if number(view, "summary", "total") != fakeaws.ChangesMalformedValid {
			t.Errorf("stored changes = %d, want the %d well-formed ones: %v", number(view, "summary", "total"), fakeaws.ChangesMalformedValid, names)
		}
		if !truth(view, "complete") {
			t.Errorf("coverage did not advance past the malformed events: gaps %v", view["gaps"])
		}
	})

	t.Run("hidden_values_never_shown", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "hidden", fakeaws.ScenarioChangesHiddenValues, options{}, prodOnly)
		r.waitPolled(150 * time.Second)
		got := r.call(http.MethodGet, "/api/admin/aws/changes?window=24h&limit=200", "")
		hidden, shown := 0, map[string]any{}
		for _, item := range list(got.json(), "items") {
			var fields []any
			for _, field := range list(item, "fields") {
				if text(field, "before_status") == "hidden" {
					hidden++
				}
				fields = append(fields, map[string]any{"field": path(field, "field"), "after": path(field, "after"), "before_status": path(field, "before_status")})
			}
			shown[text(item, "event_name")] = map[string]any{"summary": path(item, "summary"), "fields": fields}
		}
		var leaked []string
		for _, canary := range fakeaws.HiddenCanaries() {
			if strings.Contains(string(got.body), canary) {
				leaked = append(leaked, canary)
			}
		}
		e.out.put("hidden_values", map[string]any{"items": shown, "hidden_fields": hidden, "leaked": leaked, "total": number(got.json(), "summary", "total")})
		if len(leaked) > 0 {
			t.Errorf("secret values in the changes answer: %v", leaked)
		}
		if number(got.json(), "summary", "total") < int64(len(fakeaws.HiddenCanaries())) {
			t.Errorf("only %d of the %d secret-carrying changes are listed", number(got.json(), "summary", "total"), len(fakeaws.HiddenCanaries()))
		}
		if hidden == 0 {
			t.Errorf("no field is marked hidden: the secret-shaped and tag changes show no \"value hidden\" field (spec 5.16)")
		}
	})

	t.Run("routine_events_counted_not_listed", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "routine", fakeaws.ScenarioChangesRoutine, options{changesInterval: "30s"}, prodOnly)
		firstView := r.waitPolled(150 * time.Second)
		firstPoll, _ := time.Parse(time.RFC3339Nano, text(firstView, "sync", "last_poll_at"))
		// Two more polls, each serving the routine events since the last.
		r.waitPolls(2, 30*time.Second)
		now := time.Now().UTC().Truncate(time.Second)
		span := "since=" + now.Add(-24*time.Hour+time.Minute).Format(time.RFC3339) + "&until=" + now.Format(time.RFC3339)
		view := r.changes("?" + span + "&limit=200")
		lastPoll, _ := time.Parse(time.RFC3339Nano, text(view, "sync", "last_poll_at"))
		// Each read serves the routine events of the 2 h before it, so the
		// reads together served [first read − 2 h, last read).
		expectedToPoll := fakeaws.ChangesRoutineCount(firstPoll.Add(-fakeaws.ChangesRoutineSpan), lastPoll)
		routineItems, routineItemCount, listed := 0, int64(0), 0
		for _, item := range list(view, "items") {
			switch {
			case text(item, "kind") == "routine":
				routineItems++
				routineItemCount += number(item, "count")
			case strings.Contains(text(item, "event_name"), "UpdateInstanceInformation") || strings.Contains(text(item, "event_name"), "SubmitTaskStateChange"):
				listed++
			}
		}
		e.out.put("routine", map[string]any{"summary_routine": number(view, "summary", "routine"), "expected_up_to_last_poll": expectedToPoll,
			"last_poll_at": text(view, "sync", "last_poll_at"), "routine_items": routineItems, "routine_item_count": routineItemCount,
			"routine_listed_as_changes": listed, "total": number(view, "summary", "total"), "complete": view["complete"]})
		if listed > 0 {
			t.Errorf("%d routine events listed one row each", listed)
		}
		if number(view, "summary", "total") != 1 {
			t.Errorf("total = %d, want the one real change", number(view, "summary", "total"))
		}
		// The poll stamps its time a moment after it read, so one five-minute
		// boundary (13 events) may fall between the two.
		if got := number(view, "summary", "routine"); got < int64(expectedToPoll-fakeaws.ChangesRoutineInstances-1) || got > int64(expectedToPoll) {
			t.Errorf("summary routine = %d, fixture served %d routine events up to the last poll", got, expectedToPoll)
		}
		if routineItems > 0 && routineItemCount != number(view, "summary", "routine") {
			// Routine counts are hidden from the list by default; when a
			// routine row is listed, it must agree with the summary.
			t.Errorf("routine items count %d, summary says %d", routineItemCount, number(view, "summary", "routine"))
		}
	})

	t.Run("email_principals_masked", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "email", fakeaws.ScenarioChangesEmailPrincipal, options{}, prodOnly)
		view := r.waitPolled(150 * time.Second)
		body := string(r.call(http.MethodGet, "/api/admin/aws/changes?window=24h&limit=200", "").body)
		actors := map[string]any{}
		for _, item := range list(view, "items") {
			actors[text(item, "kind")+" "+text(item, "event_name")] = map[string]any{"actor": path(item, "actor"), "actor_class": path(item, "actor_class"), "count": path(item, "count")}
			if text(item, "kind") == "burst" {
				body += string(r.call(http.MethodGet, "/api/admin/aws/changes/groups/"+url.PathEscape(text(item, "id"))+"?limit=50", "").body)
			}
		}
		var leaked []string
		for _, email := range fakeaws.ChangesEmailPrincipals() {
			if strings.Contains(body, email) {
				leaked = append(leaked, email)
			}
		}
		e.out.put("email_principal", map[string]any{"actors": actors, "leaked": leaked, "people": path(view, "summary", "people"), "total": number(view, "summary", "total")})
		if len(leaked) > 0 {
			t.Errorf("e-mail principals in the changes answer: %v", leaked)
		}
		if len(itemsOf(view, "burst")) == 0 {
			t.Errorf("no burst for the failures through one permission set: %v", actors)
		}
	})

	t.Run("tied_suspect_candidates", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "tied", fakeaws.ScenarioChangesTiedSuspects, options{}, prodOnly)
		r.waitPolled(150 * time.Second)
		var view map[string]any
		poll(90*time.Second, func() bool {
			view = r.changes("?window=24h")
			return text(view, "suspect", "status") == "found"
		})
		candidates := list(view, "suspect", "candidates")
		tied, likely := 0, 0
		for _, candidate := range candidates {
			if truth(candidate, "tied") {
				tied++
			}
			if text(candidate, "confidence") == "likely" {
				likely++
			}
		}
		e.out.put("tied_suspects", map[string]any{"suspect": view["suspect"], "alarms": view["alarms"]})
		if text(view, "suspect", "status") != "found" {
			t.Fatalf("suspect status %q: %v", text(view, "suspect", "status"), view["suspect"])
		}
		if tied < 2 || likely != 0 {
			t.Errorf("candidates tied=%d likely=%d, want at least two tied and none likely: %v", tied, likely, candidates)
		}
	})

	t.Run("compare_nothing_differs", func(t *testing.T) {
		e := stateEnv(t)
		r, _ := e.boot(t, "same", fakeaws.ScenarioChangesQuiet, options{inventoryInterval: "30s"}, prodOnly)
		first := r.waitPolled(150 * time.Second)
		// One more inventory pass (every 30 s) after the first change read,
		// then two after the mark, so the compare spans re-reads of a
		// stable world on both sides.
		synced := func() time.Time { return r.syncedAt(prodAccount) }
		waitPasses(t, "inventory reads", lastPoll(first), 1, 30*time.Second, synced)
		mark := time.Now().UTC().Truncate(time.Second)
		waitPasses(t, "inventory reads", mark, 2, 30*time.Second, synced)
		got := r.call(http.MethodGet, "/api/admin/aws/changes/compare?from="+mark.Format(time.RFC3339), "")
		answer := got.json()
		e.out.put("compare_nothing_differs", map[string]any{"from": mark, "status": answer["status"], "complete": answer["complete"],
			"added": answer["added"], "removed": answer["removed"], "changed": answer["changed"], "partial": answer["partial"],
			"snapshots": answer["snapshots"], "changes_between_total": answer["changes_between_total"]})
		if got.status != http.StatusOK {
			t.Fatalf("compare = %d %.300s", got.status, got.body)
		}
		if text(answer, "status") != "ok" || !truth(answer, "complete") || len(list(answer, "added"))+len(list(answer, "removed"))+len(list(answer, "changed"))+len(list(answer, "partial")) != 0 {
			t.Errorf("compare over a stable world: status %v complete %v added %d removed %d changed %v partial %v",
				answer["status"], answer["complete"], len(list(answer, "added")), len(list(answer, "removed")), answer["changed"], answer["partial"])
		}
	})
}

// configAccount is one connectors.yaml account entry, Regions in the order
// given, assuming the fixture's reader role.
func configAccount(account string, regions ...string) string {
	return "      - account_id: \"" + account + "\"\n        display_name: qa-" + account + "\n        regions: [" + strings.Join(regions, ", ") + "]\n" +
		"        auth:\n          mode: assume_role\n          role_arn: arn:aws:iam::" + account + ":role/versus-reader\n" +
		"          external_id: ${VERSUS_AWS_EXTERNAL_ID}\n"
}

// settingsNotEnabled reports whether Settings lists region among the
// account's Regions that are not enabled.
func settingsNotEnabled(r *replica, account, region string) bool {
	for _, item := range list(r.call(http.MethodGet, "/api/admin/aws/settings", "").json(), "accounts") {
		if text(item, "account_id") != account {
			continue
		}
		for _, off := range list(item, "regions", "not_enabled") {
			if off == region {
				return true
			}
		}
	}
	return false
}

// notEnabledRegionChecks: the not-enabled Region is listed as such, its
// gaps say region_not_enabled, and it never makes the answer incomplete.
func notEnabledRegionChecks(t *testing.T, view map[string]any, regionGaps []any) {
	t.Helper()
	state := ""
	for _, region := range list(view, "regions") {
		if text(region, "region") == fakeaws.NotEnabledRegion {
			state = text(region, "state")
		}
	}
	if state != "not_enabled" {
		t.Errorf("regions[%s].state = %q, want not_enabled: %v", fakeaws.NotEnabledRegion, state, view["regions"])
	}
	for _, gap := range regionGaps {
		if text(gap, "reason") != "region_not_enabled" {
			t.Errorf("%s gap reason %v, want region_not_enabled", fakeaws.NotEnabledRegion, path(gap, "reason"))
		}
	}
	if !truth(view, "complete") {
		t.Errorf("the not-enabled Region makes the answer incomplete: accounts %v gaps %v", view["accounts"], view["gaps"])
	}
}
