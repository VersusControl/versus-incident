package storage

import "encoding/json"

var measurementFields = []string{"count", "evidence", "examples", "of", "changes", "more_changes", "history", "gaps", "rebuilt", "untracked", "partial", "pending"}

func issueMeasurements(payload json.RawMessage) json.RawMessage {
	var value map[string]json.RawMessage
	if json.Unmarshal(payload, &value) != nil || value == nil {
		return nil
	}
	result := map[string]json.RawMessage{}
	for _, key := range measurementFields {
		if item, ok := value[key]; ok {
			result[key] = item
		}
	}
	raw, _ := json.Marshal(result)
	return raw
}

func overlayIssueMeasurements(payload, measurements json.RawMessage) json.RawMessage {
	if len(measurements) == 0 {
		return payload
	}
	var value, observed map[string]json.RawMessage
	if json.Unmarshal(payload, &value) != nil || json.Unmarshal(measurements, &observed) != nil || value == nil {
		return payload
	}
	for _, key := range measurementFields {
		delete(value, key)
		if item, ok := observed[key]; ok {
			value[key] = item
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return payload
	}
	return raw
}
