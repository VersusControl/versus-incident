package utils

// internalAlertKey marks content an internal emission built. Its value
// has a type only Go code can make: content decoded from a webhook body
// holds a string, number, bool, list or map there, never this. So a
// webhook caller cannot pick an issue template, such as the Kubernetes
// issue template, which trusts its content to be built and escaped by
// Versus.
const internalAlertKey = "InternalAlert"

type internalAlertMark struct{}

// MarshalJSON stores the mark as true, so a stored incident reads cleanly.
func (internalAlertMark) MarshalJSON() ([]byte, error) { return []byte("true"), nil }

// MarkInternalAlert marks content built by Versus itself, such as a
// Kubernetes issue.
func MarkInternalAlert(content map[string]interface{}) {
	if content != nil {
		content[internalAlertKey] = internalAlertMark{}
	}
}

// IsInternalAlert reports whether Versus built the content itself.
func IsInternalAlert(content map[string]interface{}) bool {
	_, ok := content[internalAlertKey].(internalAlertMark)
	return ok
}
