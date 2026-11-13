package workflow

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel validation errors returned by Registry.Register. Callers may use
// errors.Is to branch on a specific failure.
var (
	// ErrEmptyWorkflowType is returned when a definition's Type is empty.
	ErrEmptyWorkflowType = errors.New("workflow definition has empty type")
	// ErrInvalidVersion is returned when a definition's Version is below 1.
	ErrInvalidVersion = errors.New("workflow definition version must be >= 1")
	// ErrEmptyActionName is returned when any action returned by Actions has an empty Name.
	ErrEmptyActionName = errors.New("workflow definition has action with empty name")
	// ErrDuplicateActionName is returned when two actions share the same Name within one definition.
	ErrDuplicateActionName = errors.New("workflow definition has duplicate action name")
	// ErrDuplicateDefinition is returned when a definition with the same Type and Version is already registered.
	ErrDuplicateDefinition = errors.New("workflow definition already registered")
)

// Registry is an in-memory, concurrency-safe registry of workflow Definitions
// keyed by Type+Version. Registration is immutable: a given (type, version)
// pair may be registered at most once. The zero-value Registry is not usable;
// construct one with NewRegistry.
type Registry struct {
	mu   sync.RWMutex
	defs map[string]map[int]Definition
}

// NewRegistry returns an empty Registry ready to accept definitions.
func NewRegistry() *Registry {
	return &Registry{defs: make(map[string]map[int]Definition)}
}

// Register validates and stores a Definition. It rejects definitions whose
// Type is empty, whose Version is below 1, whose Actions contain an empty or
// duplicate Name, and definitions whose (Type, Version) pair is already
// present. Register returns nil when the definition has been stored.
func (r *Registry) Register(def Definition) error {
	if def == nil {
		return ErrEmptyWorkflowType
	}
	workflowType := def.Type()
	version := def.Version()
	actions := def.Actions()

	if err := validateDefinition(workflowType, version, actions); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	byVersion, ok := r.defs[workflowType]
	if !ok {
		byVersion = make(map[int]Definition)
		r.defs[workflowType] = byVersion
	}
	if _, exists := byVersion[version]; exists {
		return fmt.Errorf("%w: type=%q version=%d", ErrDuplicateDefinition, workflowType, version)
	}
	byVersion[version] = def
	return nil
}

// Get returns the registered Definition for the given (type, version) pair and
// ok=true, or (nil, false) when no such definition is registered.
func (r *Registry) Get(workflowType string, version int) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byVersion, ok := r.defs[workflowType]
	if !ok {
		return nil, false
	}
	def, ok := byVersion[version]
	return def, ok
}

// validateDefinition performs the structural checks shared across all
// registrations. It does not acquire the registry mutex.
func validateDefinition(workflowType string, version int, actions []Action) error {
	switch {
	case workflowType == "":
		return ErrEmptyWorkflowType
	case version < 1:
		return ErrInvalidVersion
	}

	seen := make(map[string]struct{}, len(actions))
	for _, a := range actions {
		name := a.Name()
		if name == "" {
			return ErrEmptyActionName
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateActionName, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}
