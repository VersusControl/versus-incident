package collect

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	trailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// serviceTrails is the unit that reads which of the account's trails keep a
// copy of Versus's read calls. AWS keeps the first copy of every
// management event free; each further trail that records read events
// bills every call again, so the cost estimate needs the count.
const serviceTrails = model.TrailService

// TrailsService is the unit service of the trail read.
const TrailsService = serviceTrails

// TrailType is the resource type of one trail the trails unit read.
const TrailType = model.TrailType

// Read-event answers on a trail.
const (
	ReadEventsYes     = "yes"
	ReadEventsNo      = "no"
	ReadEventsUnknown = "unknown"
)

// collectTrails reads the trails that record events in the unit's Region,
// shadow trails included (a multi-Region trail from another Region, an
// organization trail), and whether each records read management events.
// An organization trail another account owns usually refuses its
// selectors to a member; it reads as unknown and is not a gap the member
// could fix.
func collectTrails(ctx context.Context, c *call) (result, error) {
	var out result
	api, err := cloudTrailClient(c)
	if err != nil {
		return out, err
	}
	include := true
	listed, err := api.DescribeTrails(ctx, &cloudtrail.DescribeTrailsInput{IncludeShadowTrails: &include})
	if err != nil {
		return out, err
	}
	for _, trail := range listed.TrailList {
		arn := deref(trail.TrailARN)
		if arn == "" {
			continue
		}
		owner := arnAccount(arn)
		readEvents := ReadEventsUnknown
		c.tried("GetEventSelectors", arn)
		selectors, err := api.GetEventSelectors(ctx, &cloudtrail.GetEventSelectorsInput{TrailName: trail.TrailARN})
		switch {
		case err == nil:
			readEvents = trailReadEvents(selectors.EventSelectors, selectors.AdvancedEventSelectors)
		case owner == "" || owner == c.unit.Account || errorCode(err) == CodeCapped:
			out.gaps = append(out.gaps, c.detailGap("GetEventSelectors", err))
		}
		resource := c.newResource(arn, TrailType, deref(trail.Name), "")
		resource.Attributes = map[string]string{model.AttrReadEvents: readEvents}
		if owner != "" && owner != c.unit.Account {
			resource.Attributes[model.AttrOwnerAccount] = owner
		}
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

// arnAccount returns the account field of an ARN, or "".
func arnAccount(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[4]
}

// trailReadEvents reports whether a trail's selectors record read
// management events. Classic selectors do when one includes management
// events with ReadWriteType All or ReadOnly. Advanced selectors do when one
// selects the Management category without limiting it to writes. A trail
// that answers no selectors at all reads as unknown.
func trailReadEvents(classic []trailtypes.EventSelector, advanced []trailtypes.AdvancedEventSelector) string {
	if len(classic) == 0 && len(advanced) == 0 {
		return ReadEventsUnknown
	}
	for _, selector := range classic {
		management := selector.IncludeManagementEvents == nil || *selector.IncludeManagementEvents
		switch selector.ReadWriteType {
		case trailtypes.ReadWriteTypeAll, trailtypes.ReadWriteTypeReadOnly, "":
			if management {
				return ReadEventsYes
			}
		}
	}
	for _, selector := range advanced {
		management, writesOnly := false, false
		for _, field := range selector.FieldSelectors {
			switch deref(field.Field) {
			case "eventCategory":
				for _, value := range field.Equals {
					if value == "Management" {
						management = true
					}
				}
			case "readOnly":
				for _, value := range field.Equals {
					if value == "false" {
						writesOnly = true
					}
				}
				for _, value := range field.NotEquals {
					if value == "true" {
						writesOnly = true
					}
				}
			}
		}
		if management && !writesOnly {
			return ReadEventsYes
		}
	}
	return ReadEventsNo
}
