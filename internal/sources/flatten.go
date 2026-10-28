package sources

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Flatten converts nested JSON into dotted keys: {"a":{"b":1}} -> {"a.b":"1"}.
// Arrays of scalars are comma-joined; arrays of objects use numeric indexes (a.0.b). Nulls become "".
func Flatten(v any) Row {
	out := Row{}
	flattenInto(out, "", v)
	return out
}

func flattenInto(out Row, prefix string, v any) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			flattenInto(out, join(k), child)
		}
	case []any:
		scalar := true
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				scalar = false
			}
		}
		if scalar {
			parts := make([]string, len(x))
			for i, e := range x {
				parts[i] = scalarString(e)
			}
			out[prefix] = strings.Join(parts, ",")
			return
		}
		for i, e := range x {
			flattenInto(out, join(strconv.Itoa(i)), e)
		}
	default:
		out[prefix] = scalarString(v)
	}
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

// ValueAt follows a dotted path ("data.employees", "meta.next", "items.0.id") into decoded JSON. An empty path is v
// itself. ok is false when a step does not exist.
func ValueAt(v any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return v, true
	}
	cur := v
	for _, step := range strings.Split(path, ".") {
		switch x := cur.(type) {
		case map[string]any:
			next, ok := x[step]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(step)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// RecordsAt returns the array at path (see ValueAt). The error names what was found instead.
func RecordsAt(v any, path string) ([]any, error) {
	got, ok := ValueAt(v, path)
	if !ok {
		return nil, fmt.Errorf("records_path %q does not exist in the response", path)
	}
	arr, ok := got.([]any)
	if !ok {
		if got == nil {
			return []any{}, nil // "data": null on an empty page
		}
		where := "the response"
		if path != "" {
			where = fmt.Sprintf("records_path %q", path)
		}
		return nil, fmt.Errorf("%s is not a JSON array (found %T); set records_path to the array of people", where, got)
	}
	return arr, nil
}
