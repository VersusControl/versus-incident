package act

import (
	"errors"
	"strings"
)

var ErrActionDenied = errors.New("action denied")

type Registry struct {
	byType map[ActionType]Adapter
}

func NewRegistry() *Registry { return &Registry{byType: make(map[ActionType]Adapter)} }

func (registry *Registry) Register(adapter Adapter) error {
	if registry == nil || adapter == nil || adapter.Type() == "" || adapter.Destructive() {
		return ErrActionDenied
	}
	name := strings.ToLower(string(adapter.Type()))
	for _, denied := range []string{"delete", "remove", "destroy", "drain", "prune", "apply", "patch", "undo", "sync", "promote", "abort", "retry"} {
		if strings.Contains(name, denied) {
			return ErrActionDenied
		}
	}
	if schemaAllowsScaleZero(adapter.Schema()) {
		return ErrActionDenied
	}
	if _, exists := registry.byType[adapter.Type()]; exists {
		return ErrActionDenied
	}
	registry.byType[adapter.Type()] = adapter
	return nil
}

func (registry *Registry) Lookup(actionType ActionType) (Adapter, bool) {
	if registry == nil {
		return nil, false
	}
	adapter, ok := registry.byType[actionType]
	return adapter, ok
}

func schemaAllowsScaleZero(schema map[string]any) bool {
	properties, _ := schema["properties"].(map[string]any)
	replicas, _ := properties["replicas"].(map[string]any)
	minimum, ok := replicas["minimum"].(int)
	if !ok {
		if number, ok := replicas["minimum"].(float64); ok {
			minimum = int(number)
			ok = true
		}
	}
	return properties != nil && replicas != nil && (!ok || minimum < 1)
}