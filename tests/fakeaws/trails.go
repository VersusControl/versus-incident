package fakeaws

import (
	"net/http"
)

// CloudTrail trails and their event selectors, for the read that counts
// how many copies of each call an account's trails keep.
//
// A world account has one multi-Region trail of its own that records every
// management event, read events included: the free first copy, nothing
// extra. The estate is shaped after a landing zone: every account sees the
// organization trail the management account owns, as a shadow trail whose
// selectors a member cannot read. The workloads account also keeps its own
// audit trail of read and write events (one extra copy), and the data
// account a trail of write events only.

func init() {
	worldHandlers["cloudtrail:DescribeTrails"] = (*Server).worldTrails
	worldHandlers["cloudtrail:GetEventSelectors"] = (*Server).worldEventSelectors
	registerEstate(map[string]estateHandler{
		"cloudtrail:DescribeTrails":    (*Server).estateTrails,
		"cloudtrail:GetEventSelectors": (*Server).estateEventSelectors,
	})
}

// Trail names tests assert on.
const (
	WorldTrailName       = "management-events"
	EstateOrgTrailARN    = "arn:aws:cloudtrail:us-east-1:" + EstateManagement + ":trail/org-trail"
	EstateAuditTrailName = "workloads-audit"
	EstateWritesTrail    = "data-writes"
)

type fakeTrail struct {
	name, arn, home    string
	multiRegion, org   bool
	readWrite          string
	advancedWritesOnly bool
}

func (trail fakeTrail) json() map[string]any {
	return map[string]any{"Name": trail.name, "TrailARN": trail.arn, "HomeRegion": trail.home,
		"IsMultiRegionTrail": trail.multiRegion, "IsOrganizationTrail": trail.org, "S3BucketName": "trail-logs"}
}

func worldTrailList(account string) []fakeTrail {
	return []fakeTrail{{name: WorldTrailName, arn: "arn:aws:cloudtrail:us-east-1:" + account + ":trail/" + WorldTrailName,
		home: "us-east-1", multiRegion: true, readWrite: "All"}}
}

func (s *Server) worldTrails(writer http.ResponseWriter, request awsRequest, w world) {
	writeTrails(writer, worldTrailList(w.account), request.region)
}

func (s *Server) worldEventSelectors(writer http.ResponseWriter, request awsRequest, w world) {
	writeEventSelectors(writer, request, worldTrailList(w.account), jsonString(jsonBody(request), "TrailName"), nil)
}

func estateTrailList(call estateCall) []fakeTrail {
	trails := []fakeTrail{{name: "org-trail", arn: EstateOrgTrailARN, home: EstateGlobalRegion, multiRegion: true, org: true, readWrite: "All"}}
	switch call.account {
	case EstateWorkloads:
		trails = append(trails, fakeTrail{name: EstateAuditTrailName, arn: "arn:aws:cloudtrail:" + EstateHomeRegion + ":" + call.account + ":trail/" + EstateAuditTrailName,
			home: EstateHomeRegion, multiRegion: true, readWrite: "All"})
	case EstateData:
		trails = append(trails, fakeTrail{name: EstateWritesTrail, arn: "arn:aws:cloudtrail:" + EstateHomeRegion + ":" + call.account + ":trail/" + EstateWritesTrail,
			home: EstateHomeRegion, multiRegion: true, advancedWritesOnly: true})
	}
	return trails
}

func (s *Server) estateTrails(writer http.ResponseWriter, call estateCall) {
	writeTrails(writer, estateTrailList(call), call.region)
}

func (s *Server) estateEventSelectors(writer http.ResponseWriter, call estateCall) {
	name := jsonString(jsonBody(call.request), "TrailName")
	if name == EstateOrgTrailARN {
		// A member account cannot read the selectors of the organization
		// trail the management account owns.
		estateError(writer, call, http.StatusBadRequest, "TrailNotFoundException", "Unknown trail for this account.")
		return
	}
	writeEventSelectors(writer, call.request, estateTrailList(call), name, func(code string) {
		estateError(writer, call, http.StatusBadRequest, code, "Unknown trail for this account.")
	})
}

// writeTrails answers DescribeTrails with shadow trails: every
// multi-Region trail, and a single-Region trail only in its home Region.
func writeTrails(writer http.ResponseWriter, trails []fakeTrail, region string) {
	list := make([]any, 0, len(trails))
	for _, trail := range trails {
		if trail.multiRegion || trail.home == region {
			list = append(list, trail.json())
		}
	}
	writeJSONBody(writer, mustJSON(map[string]any{"trailList": list}))
}

func writeEventSelectors(writer http.ResponseWriter, request awsRequest, trails []fakeTrail, name string, fail func(string)) {
	for _, trail := range trails {
		if trail.arn != name && trail.name != name {
			continue
		}
		body := map[string]any{"TrailARN": trail.arn}
		if trail.advancedWritesOnly {
			body["AdvancedEventSelectors"] = []any{map[string]any{"Name": "Management writes", "FieldSelectors": []any{
				map[string]any{"Field": "eventCategory", "Equals": []any{"Management"}},
				map[string]any{"Field": "readOnly", "Equals": []any{"false"}},
			}}}
		} else {
			body["EventSelectors"] = []any{map[string]any{"ReadWriteType": trail.readWrite, "IncludeManagementEvents": true, "DataResources": []any{}}}
		}
		writeJSONBody(writer, mustJSON(body))
		return
	}
	if fail != nil {
		fail("TrailNotFoundException")
		return
	}
	writeErrorFor(writer, request, http.StatusBadRequest, "TrailNotFoundException", "Unknown trail for this account.")
}
