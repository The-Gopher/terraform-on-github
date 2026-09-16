package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Registry loads TrustedRefs from a local JSON file.
type Registry struct {
	path string
}

func NewRegistry(path string) *Registry {
	return &Registry{path: path}
}

func (r *Registry) Load() (TrustedRefs, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(TrustedRefs), nil
		}
		return nil, fmt.Errorf("registry read: %w", err)
	}

	var refs TrustedRefs
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, fmt.Errorf("registry decode: %w", err)
	}

	return refs, nil
}
