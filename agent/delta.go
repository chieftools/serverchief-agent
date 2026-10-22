package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"go.chief.tools/serverchief-agent/inventory"
)

// itemsField holds the keyed list of a delta dataset.
const itemsField = "items"

// buildDelta computes the delta from the acknowledged payload base to
// current for a keyed dataset. It reports ok = false when a delta cannot
// express the change (a top-level field other than items changed) or would
// not be smaller than the full payload.
func buildDelta(base, current []byte, keyFields []string) (delta inventory.Delta[map[string]any], ok bool, err error) {
	baseTop, baseItems, err := splitKeyed(base)
	if err != nil {
		return delta, false, fmt.Errorf("base payload: %w", err)
	}
	currentTop, currentItems, err := splitKeyed(current)
	if err != nil {
		return delta, false, fmt.Errorf("current payload: %w", err)
	}

	baseRest, err := inventory.Canonical(withoutItems(baseTop))
	if err != nil {
		return delta, false, err
	}
	currentRest, err := inventory.Canonical(withoutItems(currentTop))
	if err != nil {
		return delta, false, err
	}
	if !bytes.Equal(baseRest, currentRest) {
		return delta, false, nil
	}

	delta, err = inventory.Diff(baseItems, currentItems, itemKey(currentTop, keyFields))
	if err != nil {
		return delta, false, err
	}
	encoded, err := inventory.Canonical(delta)
	if err != nil {
		return delta, false, err
	}
	return delta, len(encoded) < len(current), nil
}

// itemKey joins keyFields with "/", reading a field from the payload's top
// level when an item lacks it.
func itemKey(top map[string]any, keyFields []string) func(map[string]any) string {
	return func(item map[string]any) string {
		parts := make([]string, len(keyFields))
		for index, field := range keyFields {
			value, ok := item[field]
			if !ok {
				value = top[field]
			}
			if value != nil {
				parts[index] = fmt.Sprint(value)
			}
		}
		return strings.Join(parts, "/")
	}
}

func splitKeyed(payload []byte) (map[string]any, []map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var top map[string]any
	if err := decoder.Decode(&top); err != nil {
		return nil, nil, err
	}
	raw, _ := top[itemsField].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, element := range raw {
		item, ok := element.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s holds a non-object", itemsField)
		}
		items = append(items, item)
	}
	return top, items, nil
}

func withoutItems(top map[string]any) map[string]any {
	rest := make(map[string]any, len(top))
	for key, value := range top {
		if key != itemsField {
			rest[key] = value
		}
	}
	return rest
}
