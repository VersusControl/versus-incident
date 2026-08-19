package services

import "github.com/VersusControl/versus-incident/pkg/utils"

// attribution.go — the single shared reader for the two labels every
// suppression and grouping decision keys off: service and severity. Real
// payloads nest them below the top level, so a top-level-only read blanks them:
// a CloudWatch alarm arriving via SNS carries them in Trigger.Dimensions[], and
// an Alertmanager notification carries them in labels/commonLabels. A blank
// severity is a safety problem downstream — the enterprise priority floor keys
// off it — so both OSS and the enterprise interceptor read through here and
// attribute identically.
//
// The lookup itself lives in pkg/utils so the storage layer and the analyze
// tools — which cannot import this package — read through the same one
// implementation.

// ExtractService resolves the owning service from a free-form content map:
// top-level keys first, then the nested Alertmanager label maps, then the
// CloudWatch alarm's Trigger.Dimensions[]. Empty when the payload names no
// service.
func ExtractService(content map[string]interface{}) string {
	return utils.ExtractService(content)
}

// ExtractSeverity resolves an alert's severity from a free-form content map
// using the same top-level → nested-labels → CloudWatch-dimensions order as
// ExtractService. Only real severity fields count; an agent verdict is not one,
// because this value drives the priority floor and the dedup fingerprint. Empty
// when the payload carries no severity; callers decide what an unlabelled alert
// means.
func ExtractSeverity(content map[string]interface{}) string {
	return utils.ExtractSeverity(content)
}

// ExtractTitle resolves an alert's human-facing title from a free-form content
// map: top-level keys (including CloudWatch's AlarmName) first, then the nested
// Alertmanager label maps. The durable incident Title, the report's title
// fallback and the emit fingerprint all read through here, so a payload is
// titled and fingerprinted off the same value. Empty when the payload names no
// title.
func ExtractTitle(content map[string]interface{}) string {
	return utils.ExtractTitle(content)
}
