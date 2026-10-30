package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
)

type inventoryListValidationError struct{}

func (*inventoryListValidationError) Error() string {
	return "kubernetes: invalid LIST response envelope"
}
func (*inventoryListValidationError) Unwrap() error { return ErrIncompleteInventory }

func invalidInventoryList() error { return &inventoryListValidationError{} }

// Ordinary LIST servers may omit type/version and pagination metadata, but an
// explicit items array is required. Negotiated metadata lists can encode a
// complete empty collection as null, with versioned ListMeta proving its shape.
func validateInventoryListEnvelope(data []byte, metadataOnly bool) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return invalidInventoryList()
	}
	stringField := func(fields map[string]json.RawMessage, name string, required bool, want string) bool {
		raw, exists := fields[name]
		if !exists {
			return !required
		}
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return false
		}
		return want == "" || value == want
	}
	kind, version := "", ""
	if metadataOnly {
		kind, version = "PartialObjectMetadataList", "meta.k8s.io/v1"
	}
	if !stringField(envelope, "kind", metadataOnly, kind) || !stringField(envelope, "apiVersion", metadataOnly, version) {
		return invalidInventoryList()
	}
	var metadata map[string]json.RawMessage
	if raw, exists := envelope["metadata"]; exists {
		if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
			return invalidInventoryList()
		}
		if raw, exists := metadata["continue"]; exists {
			var token string
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &token) != nil || len(token) > 4096 {
				return invalidInventoryList()
			}
		}
		if !stringField(metadata, "resourceVersion", false, "") {
			return invalidInventoryList()
		}
	}
	raw, exists := envelope["items"]
	if !exists {
		return invalidInventoryList()
	}
	var items []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// The metadata transform uses a nil slice for an empty collection.
		// Do not generalize that encoding to untyped or incomplete LISTs.
		var resourceVersion, token string
		if !metadataOnly || metadata == nil || json.Unmarshal(metadata["resourceVersion"], &resourceVersion) != nil || resourceVersion == "" {
			return invalidInventoryList()
		}
		if continuation, exists := metadata["continue"]; exists && (json.Unmarshal(continuation, &token) != nil || token != "") {
			return invalidInventoryList()
		}
		if remaining, exists := metadata["remainingItemCount"]; exists {
			var count int64
			if bytes.Equal(bytes.TrimSpace(remaining), []byte("null")) || json.Unmarshal(remaining, &count) != nil || count != 0 {
				return invalidInventoryList()
			}
		}
	} else if json.Unmarshal(raw, &items) != nil || items == nil {
		return invalidInventoryList()
	}
	for _, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			return invalidInventoryList()
		}
		if metadataOnly {
			for key := range item {
				if key != "kind" && key != "apiVersion" && key != "metadata" {
					return invalidInventoryList()
				}
			}
			if !stringField(item, "kind", false, "PartialObjectMetadata") || !stringField(item, "apiVersion", false, "meta.k8s.io/v1") {
				return invalidInventoryList()
			}
		}
		var metadata map[string]json.RawMessage
		raw, exists := item["metadata"]
		if !exists || json.Unmarshal(raw, &metadata) != nil || metadata == nil {
			return invalidInventoryList()
		}
		for _, name := range []string{"name", "namespace", "uid", "resourceVersion"} {
			if !stringField(metadata, name, false, "") {
				return invalidInventoryList()
			}
		}
	}
	return nil
}

type validatedInventoryList struct {
	target       any
	metadataOnly bool
}

func (list *validatedInventoryList) UnmarshalJSON(data []byte) error {
	if err := validateInventoryListEnvelope(data, list.metadataOnly); err != nil {
		return err
	}
	if err := json.Unmarshal(data, list.target); err != nil {
		return invalidInventoryList()
	}
	return nil
}

func (service *Service) inventoryListRead(ctx context.Context, path string, target any, metadataOnly bool) error {
	return service.inventoryRead(ctx, path, &validatedInventoryList{target: target, metadataOnly: metadataOnly}, metadataOnly)
}
