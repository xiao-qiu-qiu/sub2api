package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResolveConfigurationUpdates lowers append-only Responses settings into the
// current BPS request. Callers enforcing request policies must apply them after
// this step, so historical updates cannot override an administrator's limits.
// Ciphertext items and the order of all non-configuration items are retained.
func ResolveConfigurationUpdates(raw []byte) ([]byte, bool, error) {
	body, changed, _, err := ResolveConfigurationUpdatesWithEffort(raw)
	return body, changed, err
}

// ResolveConfigurationUpdatesWithEffort also reports whether history explicitly
// supplied an effort. A text-only update must not reapply effort mappings.
func ResolveConfigurationUpdatesWithEffort(raw []byte) ([]byte, bool, bool, error) {
	var source object
	if decode(raw, &source) != nil || source == nil {
		return nil, false, false, fmt.Errorf("invalid Basispoints request JSON")
	}
	changed, effortUpdated, err := resolveConfigurationUpdatesWithEffort(source)
	if err != nil {
		return nil, false, false, err
	}
	if !changed {
		return raw, false, false, nil
	}
	body, err := json.Marshal(source)
	return body, true, effortUpdated, err
}

func resolveConfigurationUpdates(source object) (bool, error) {
	changed, _, err := resolveConfigurationUpdatesWithEffort(source)
	return changed, err
}

func resolveConfigurationUpdatesWithEffort(source object) (bool, bool, error) {
	input, ok := source["input"].([]any)
	if !ok {
		return false, false, nil
	}
	var filtered []any
	effortUpdated := false
	for index, raw := range input {
		item, _ := raw.(object)
		if text(item["type"]) != "configuration_update" {
			if filtered != nil {
				filtered = append(filtered, raw)
			}
			continue
		}
		if filtered == nil {
			filtered = make([]any, 0, len(input)-1)
			filtered = append(filtered, input[:index]...)
		}
		if err := applyConfigurationUpdate(source, item); err != nil {
			return false, false, fmt.Errorf("basispoints input[%d].configuration_update: %w", index, err)
		}
		if reasoning, ok := item["reasoning"].(object); ok && reasoning["effort"] != nil {
			effortUpdated = true
		}
	}
	if filtered == nil {
		return false, false, nil
	}
	source["input"] = filtered
	return true, effortUpdated, nil
}

func applyConfigurationUpdate(source, item object) error {
	for key := range item {
		switch key {
		case "type", "id", "reasoning", "text":
		default:
			return fmt.Errorf("unsupported field %q", key)
		}
	}
	if id := item["id"]; id != nil {
		if _, ok := id.(string); !ok {
			return fmt.Errorf("id must be a string")
		}
	}
	if raw := item["reasoning"]; raw != nil {
		reasoning, ok := raw.(object)
		if !ok {
			return fmt.Errorf("reasoning must be an object")
		}
		for key := range reasoning {
			if key != "effort" {
				return fmt.Errorf("unsupported reasoning field %q", key)
			}
		}
		if value := reasoning["effort"]; value != nil {
			effort, ok := value.(string)
			if !ok || strings.TrimSpace(effort) == "" {
				return fmt.Errorf("reasoning.effort must be a nonempty string")
			}
			if _, err := NormalizeEffort(effort); err != nil {
				return err
			}
			if err := setConfigurationField(source, "reasoning", "effort", effort); err != nil {
				return err
			}
			// The gateway checks every legacy spelling. Keep existing aliases in
			// sync so an obsolete initial value cannot deny or remap this turn.
			if _, exists := source["reasoning_effort"]; exists {
				source["reasoning_effort"] = effort
			}
			if output, ok := source["output_config"].(object); ok {
				if _, exists := output["effort"]; exists {
					output["effort"] = effort
				}
			}
		}
	}
	if raw := item["text"]; raw != nil {
		config, ok := raw.(object)
		if !ok {
			return fmt.Errorf("text must be an object")
		}
		for key := range config {
			if key != "format" {
				return fmt.Errorf("unsupported text field %q", key)
			}
		}
		if format := config["format"]; format != nil {
			if _, err := prepareStructuredOutput(config); err != nil {
				return err
			}
			if err := setConfigurationField(source, "text", "format", format); err != nil {
				return err
			}
		}
	}
	return nil
}

func setConfigurationField(source object, section, key string, value any) error {
	config, ok := source[section].(object)
	if source[section] != nil && !ok {
		return fmt.Errorf("request %s must be an object", section)
	}
	if config == nil {
		config = make(object)
		source[section] = config
	}
	config[key] = value
	return nil
}
