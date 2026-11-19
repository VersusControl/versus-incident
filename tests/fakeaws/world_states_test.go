package fakeaws

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stateCall sends one signed-looking request: service and region in the
// credential scope, an optional session token naming the account.
func stateCall(t *testing.T, server *Server, scenario, service, region, token, contentType, target, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://"+service+"."+region+".amazonaws.com/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=ASIAKEY/20261119/"+region+"/"+service+"/aws4_request, SignedHeaders=host, Signature=x")
	request.Header.Set("Content-Type", contentType)
	if target != "" {
		request.Header.Set("X-Amz-Target", target)
	}
	if token != "" {
		request.Header.Set("X-Amz-Security-Token", token)
	}
	if scenario != "" {
		request.Header.Set(ScenarioHeader, scenario)
	}
	response, err := server.HTTPClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

const formType = "application/x-www-form-urlencoded"

type lookedUp struct {
	EventID         string `json:"EventId"`
	EventName       string
	EventTime       *float64
	CloudTrailEvent string
}

func lookupAll(t *testing.T, server *Server, scenario, region, token string) []lookedUp {
	t.Helper()
	var out []lookedUp
	next := ""
	for range 100 {
		body := `{"MaxResults":50`
		if next != "" {
			body += `,"NextToken":` + quote(next)
		}
		status, answer := stateCall(t, server, scenario, "cloudtrail", region, token, "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", body+"}")
		if status != http.StatusOK {
			t.Fatalf("lookup = %d %s", status, answer)
		}
		var page struct {
			Events    []lookedUp
			NextToken string
		}
		if err := json.Unmarshal([]byte(answer), &page); err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Events...)
		if page.NextToken == "" {
			return out
		}
		next = page.NextToken
	}
	t.Fatal("paging did not end")
	return nil
}

func TestRoleSessionExpiredRefusesOnlyTheRevokedAccount(t *testing.T) {
	server := New(t, ScenarioBadDeploy)
	assume := func(account string) (int, string) {
		form := url.Values{"Action": {"AssumeRole"}, "RoleArn": {"arn:aws:iam::" + account + ":role/versus-reader"},
			"RoleSessionName": {"versus"}, "ExternalId": {"vrs-ext-abcdefgh1234"}}
		return stateCall(t, server, ScenarioRoleSessionExpired, "sts", "us-east-1", "", formType, "", form.Encode())
	}
	if status, body := assume(RevokedAccount); status != http.StatusForbidden || !strings.Contains(body, "AccessDenied") || !strings.Contains(body, "sts:AssumeRole") {
		t.Fatalf("revoked assume = %d %s", status, body)
	}
	if status, body := assume("111122223333"); status != http.StatusOK || !strings.Contains(body, "<SessionToken>") {
		t.Fatalf("other account assume = %d %s", status, body)
	}
	identity := url.Values{"Action": {"GetCallerIdentity"}}.Encode()
	if status, body := stateCall(t, server, ScenarioRoleSessionExpired, "sts", "us-east-1", sessionToken(RevokedAccount), formType, "", identity); status != http.StatusForbidden || !strings.Contains(body, "<Code>ExpiredToken</Code>") {
		t.Fatalf("revoked session identity = %d %s", status, body)
	}
	status, body := stateCall(t, server, ScenarioRoleSessionExpired, "cloudtrail", "us-east-1", sessionToken(RevokedAccount), "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}")
	if status != http.StatusForbidden || !strings.Contains(body, "ExpiredTokenException") {
		t.Fatalf("revoked session lookup = %d %s", status, body)
	}
	if status, body := stateCall(t, server, ScenarioRoleSessionExpired, "cloudtrail", "us-east-1", sessionToken("111122223333"), "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}"); status != http.StatusOK {
		t.Fatalf("other account lookup = %d %s", status, body)
	}
	// The same session works again outside the scenario.
	if status, body := stateCall(t, server, "", "cloudtrail", "us-east-1", sessionToken(RevokedAccount), "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}"); status != http.StatusOK {
		t.Fatalf("lookup outside the scenario = %d %s", status, body)
	}
}

func TestRegionNotEnabledRefusesEveryCallInTheRegion(t *testing.T) {
	server := New(t, ScenarioBadDeploy)
	token := sessionToken("111122223333")
	regions := func(all bool) string {
		form := url.Values{"Action": {"DescribeRegions"}, "Version": {"2016-11-15"}}
		if all {
			form.Set("AllRegions", "true")
		}
		status, body := stateCall(t, server, ScenarioRegionNotEnabled, "ec2", "us-east-1", token, formType, "", form.Encode())
		if status != http.StatusOK {
			t.Fatalf("describe regions = %d %s", status, body)
		}
		return body
	}
	if body := regions(true); !strings.Contains(body, "<regionName>"+NotEnabledRegion+"</regionName>") || !strings.Contains(body, "<optInStatus>not-opted-in</optInStatus>") {
		t.Fatalf("all regions lack the not-enabled region: %s", body)
	}
	if body := regions(false); strings.Contains(body, NotEnabledRegion) {
		t.Fatalf("enabled regions list the not-enabled region: %s", body)
	}
	cases := []struct {
		service, contentType, target, body, code string
	}{
		{"ec2", formType, "", "Action=DescribeInstances&Version=2016-11-15", "AuthFailure"},
		{"cloudtrail", "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}", "UnrecognizedClientException"},
		{"sns", formType, "", "Action=ListTopics&Version=2010-03-31", "InvalidClientTokenId"},
		{"sts", formType, "", "Action=GetCallerIdentity", "InvalidClientTokenId"},
	}
	for _, c := range cases {
		status, body := stateCall(t, server, ScenarioRegionNotEnabled, c.service, NotEnabledRegion, token, c.contentType, c.target, c.body)
		if status < 400 || !strings.Contains(body, c.code) {
			t.Errorf("%s in %s = %d %s, want %s", c.service, NotEnabledRegion, status, body, c.code)
		}
		if status, body := stateCall(t, server, ScenarioRegionNotEnabled, c.service, "eu-west-1", token, c.contentType, c.target, c.body); status != http.StatusOK {
			t.Errorf("%s in eu-west-1 = %d %.200s", c.service, status, body)
		}
	}
	// As the server's own scenario it holds under any per-request scenario.
	base := New(t, ScenarioRegionNotEnabled)
	if status, _ := stateCall(t, base, ScenarioChangesQuiet, "cloudtrail", NotEnabledRegion, token, "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}"); status != http.StatusBadRequest {
		t.Fatalf("layered change scenario reached the not-enabled region: %d", status)
	}
}

func TestEventIDsAreUniquePerAccountAndRegion(t *testing.T) {
	server := New(t, ScenarioChangesErrorBurst)
	seen := map[string]string{}
	for _, pair := range [][2]string{{"111122223333", "us-east-1"}, {"111122223333", "eu-west-1"}, {RevokedAccount, "us-east-1"}} {
		for _, event := range lookupAll(t, server, "", pair[1], sessionToken(pair[0])) {
			if owner, ok := seen[event.EventID]; ok {
				t.Fatalf("event %s served to %s and %v", event.EventID, owner, pair)
			}
			seen[event.EventID] = pair[0] + "/" + pair[1]
		}
	}
	if len(seen) != 3*(ChangesBurstSize+1) {
		t.Fatalf("ids = %d", len(seen))
	}
}

func TestLateArrivalServesOnlyDeliveredEvents(t *testing.T) {
	server := New(t, ScenarioChangesLateArrival)
	now := time.Date(2026, 11, 19, 12, 0, 30, 0, time.UTC)
	server.SetClock(func() time.Time { return now })
	first := lookupAll(t, server, "", "us-east-1", sessionToken("111122223333"))
	newest := 0.0
	for _, event := range first {
		if event.EventTime != nil && *event.EventTime > newest && strings.HasPrefix(event.EventID, "late-") {
			newest = *event.EventTime
		}
	}
	if want := float64(now.Add(-ChangesLateDelay).Truncate(time.Minute).Unix()); newest != want {
		t.Fatalf("newest delivered event at %v, want %v", newest, want)
	}
	// Five minutes later five more events are delivered, all older than
	// now - 15 min, and the earlier ones keep their IDs.
	now = now.Add(5 * time.Minute)
	second := lookupAll(t, server, "", "us-east-1", sessionToken("111122223333"))
	ids := map[string]bool{}
	for _, event := range second {
		ids[event.EventID] = true
	}
	added := 0
	for id := range ids {
		found := false
		for _, event := range first {
			found = found || event.EventID == id
		}
		if !found {
			added++
		}
	}
	if added != 5 {
		t.Fatalf("newly delivered = %d, want 5", added)
	}
}

func TestMalformedScenarioServesEachMalformedShape(t *testing.T) {
	server := New(t, ScenarioChangesMalformed)
	events := lookupAll(t, server, "", "us-east-1", sessionToken("111122223333"))
	noID, noTime, nul, raw, valid := 0, 0, 0, 0, 0
	for _, event := range events {
		switch {
		case event.EventID == "":
			noID++
		case event.EventTime == nil:
			noTime++
		case strings.ContainsRune(event.EventName, 0):
			nul++
		case event.CloudTrailEvent == "{not json":
			raw++
		default:
			valid++
		}
	}
	if noID != 1 || noTime != 1 || nul != 1 || raw != 1 || valid != ChangesMalformedValid {
		t.Fatalf("no id %d, no time %d, nul %d, raw %d, valid %d", noID, noTime, nul, raw, valid)
	}
}

func TestHiddenValuesAndEmailPrincipalsArePlanted(t *testing.T) {
	server := New(t, ScenarioChangesHiddenValues)
	var text strings.Builder
	for _, region := range []string{"us-east-1"} {
		for _, event := range lookupAll(t, server, "", region, sessionToken("111122223333")) {
			text.WriteString(event.CloudTrailEvent)
		}
	}
	for _, canary := range HiddenCanaries() {
		if !strings.Contains(text.String(), canary) {
			t.Errorf("canary %s not planted", canary)
		}
	}
	text.Reset()
	for _, event := range lookupAll(t, server, ScenarioChangesEmailPrincipal, "us-east-1", sessionToken("111122223333")) {
		text.WriteString(event.CloudTrailEvent)
	}
	for _, email := range ChangesEmailPrincipals() {
		if !strings.Contains(text.String(), email) {
			t.Errorf("principal %s not planted", email)
		}
	}
	// No other scenario carries them.
	text.Reset()
	for _, event := range lookupAll(t, server, ScenarioChangesQuiet, "us-east-1", sessionToken("111122223333")) {
		text.WriteString(event.CloudTrailEvent)
	}
	for _, value := range append(HiddenCanaries(), ChangesEmailPrincipals()...) {
		if strings.Contains(text.String(), value) {
			t.Errorf("quiet scenario carries %s", value)
		}
	}
}

func TestRoutineEventsAreStableAcrossReads(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 3, 0, 0, time.UTC)
	from := now.Add(-ChangesRoutineSpan)
	// 24 five-minute boundaries × 12 instances, 12 ten-minute boundaries.
	if got := ChangesRoutineCount(from, now); got != 24*ChangesRoutineInstances+12 {
		t.Fatalf("routine count = %d", got)
	}
	first := routineEvents("111122223333", from, now)
	later := routineEvents("111122223333", from.Add(time.Minute), now.Add(time.Minute))
	at := map[string]time.Time{}
	for _, event := range first {
		at[event.id] = event.at
	}
	for _, event := range later {
		if when, ok := at[event.id]; ok && !when.Equal(event.at) {
			t.Fatalf("event %s moved from %s to %s", event.id, when, event.at)
		}
	}
}

func TestTiedSuspectsKeepTheBadDeployWorld(t *testing.T) {
	w := world{scenario: ScenarioChangesTiedSuspects, account: "111122223333", region: "us-east-1", now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	if _, firing := w.alarmFiring(); !firing || w.revision() != WorldBadRevision {
		t.Fatal("tied scenario lost the bad-deploy alarm or revision")
	}
	updates := 0
	for _, event := range w.trail() {
		if event.name == "UpdateService" && !event.at.Before(BadDeployStart(w.now)) {
			updates++
		}
	}
	if updates != 2 {
		t.Fatalf("UpdateService events = %d, want 2", updates)
	}
}

func TestRegionOptInUnrecordedIsNeverListed(t *testing.T) {
	server := New(t, ScenarioRegionOptInUnrecorded)
	token := sessionToken("111122223333")
	form := url.Values{"Action": {"DescribeRegions"}, "Version": {"2016-11-15"}, "AllRegions": {"true"}}
	status, body := stateCall(t, server, "", "ec2", "us-east-1", token, formType, "", form.Encode())
	if status != http.StatusOK || strings.Contains(body, NotEnabledRegion) {
		t.Fatalf("describe regions = %d, lists %s: %s", status, NotEnabledRegion, body)
	}
	if status, body := stateCall(t, server, "", "ec2", NotEnabledRegion, token, formType, "", "Action=DescribeInstances&Version=2016-11-15"); status != http.StatusUnauthorized || !strings.Contains(body, "OptInRequired") {
		t.Fatalf("ec2 in %s = %d %s", NotEnabledRegion, status, body)
	}
	if status, body := stateCall(t, server, "", "cloudtrail", NotEnabledRegion, token, "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}"); status != http.StatusUnauthorized || !strings.Contains(body, "OptInRequired") {
		t.Fatalf("cloudtrail in %s = %d %s", NotEnabledRegion, status, body)
	}
	if status, _ := stateCall(t, server, "", "cloudtrail", "us-east-1", token, "application/x-amz-json-1.1", "CloudTrail_20131101.LookupEvents", "{}"); status != http.StatusOK {
		t.Fatalf("cloudtrail in us-east-1 = %d", status)
	}
}

// changes-busy anchors its history once: a window read again later holds
// the same events at the same times, so a backfill split over many reads
// never sees an event move between windows.
func TestBusyEventTimesAreAbsoluteAcrossReads(t *testing.T) {
	server := New(t, ScenarioChangesBusy)
	start := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	window := func() map[string]float64 {
		t.Helper()
		body := `{"MaxResults":50,"StartTime":` + jsonNumber(start.Add(-10*time.Minute)) + `,"EndTime":` + jsonNumber(start) + `}`
		status, answer := worldRequest(t, server, http.MethodPost, "/", body, map[string]string{"service": "cloudtrail", "X-Amz-Target": "CloudTrail_20131101.LookupEvents"})
		if status != http.StatusOK {
			t.Fatalf("lookup = %d %s", status, answer)
		}
		var page struct {
			Events    []lookedUp
			NextToken string
		}
		if err := json.Unmarshal([]byte(answer), &page); err != nil {
			t.Fatal(err)
		}
		if page.NextToken != "" {
			t.Fatalf("a 10-minute window needs more than one page: %d events", len(page.Events))
		}
		at := map[string]float64{}
		for _, event := range page.Events {
			if event.EventTime == nil {
				t.Fatalf("event %s has no time", event.EventID)
			}
			at[event.EventID] = *event.EventTime
		}
		return at
	}
	server.SetClock(func() time.Time { return start })
	first := window()
	if len(first) == 0 {
		t.Fatal("no busy events in the 10 minutes before the world start")
	}
	server.SetClock(func() time.Time { return start.Add(7 * time.Minute) })
	later := window()
	if len(later) != len(first) {
		t.Fatalf("window holds %d events on the first read and %d seven minutes later", len(first), len(later))
	}
	for id, when := range first {
		if got, ok := later[id]; !ok || got != when {
			t.Fatalf("event %s moved from %v to %v (present %t)", id, when, got, ok)
		}
	}
}
