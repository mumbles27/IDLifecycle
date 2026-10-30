package api

import (
	"encoding/json"
	"reflect"
	"sort"
)

// Audit detail for console changes: what changed (from/to) on an update, and the main fields on a create or delete.
// Every object is compared twice, as stored (raw) and as the API shows it (masked): a field whose two forms differ is
// a secret (a password, DSN, API key or client secret, encrypted in the store), so only the fact that it changed is
// recorded, never a value or its ciphertext.

// hiddenValue stands for a secret in an audit snapshot.
const hiddenValue = "(hidden)"

// auditNoise are bookkeeping fields that are not part of what a person changed.
var auditNoise = map[string]bool{"id": true, "created_at": true, "updated_at": true, "last_sync_at": true, "last_status": true,
	"last_error": true, "last_login_at": true, "password_hash": true}

// flattenJSON turns v (marshalled to JSON) into dotted paths to leaf values; arrays are leaves.
func flattenJSON(v any) map[string]any {
	out := map[string]any{}
	if v == nil {
		return out
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	var m any
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	var walk func(prefix string, x any)
	walk = func(prefix string, x any) {
		if xm, ok := x.(map[string]any); ok {
			if len(xm) == 0 && prefix != "" {
				out[prefix] = map[string]any{}
			}
			for k, val := range xm {
				if auditNoise[k] {
					continue
				}
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, val)
			}
			return
		}
		if prefix != "" {
			out[prefix] = x
		}
	}
	walk("", m)
	return out
}

func sameJSON(a, b any) bool {
	// An absent field, null and "" all mean "empty" to a person reading the history.
	if isEmptyJSON(a) && isEmptyJSON(b) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func isEmptyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// auditChanges returns {"path": {"from": x, "to": y}} for every field that differs between the old and the new object;
// secrets as {"path": {"changed": true}}. Pass the masked forms (what the API returns) next to the raw ones; for objects
// without secrets pass the same value twice.
func auditChanges(oldRaw, newRaw, oldMasked, newMasked any) map[string]any {
	ro, rn, mo, mn := flattenJSON(oldRaw), flattenJSON(newRaw), flattenJSON(oldMasked), flattenJSON(newMasked)
	paths := map[string]bool{}
	for p := range ro {
		paths[p] = true
	}
	for p := range rn {
		paths[p] = true
	}
	out := map[string]any{}
	for p := range paths {
		if sameJSON(ro[p], rn[p]) {
			continue
		}
		if !sameJSON(ro[p], mo[p]) || !sameJSON(rn[p], mn[p]) {
			out[p] = map[string]any{"changed": true}
			continue
		}
		out[p] = map[string]any{"from": mo[p], "to": mn[p]}
	}
	return out
}

// auditSnapshot returns the object's fields as dotted paths (empty ones left out), secrets as "(hidden)".
func auditSnapshot(raw, masked any) map[string]any {
	r, m := flattenJSON(raw), flattenJSON(masked)
	out := map[string]any{}
	for p, v := range r {
		if isEmptyJSON(v) {
			continue
		}
		if !sameJSON(v, m[p]) {
			out[p] = hiddenValue
			continue
		}
		out[p] = v
	}
	return out
}

// changedPaths lists the keys of an auditChanges result, sorted (for the short "changed" list).
func changedPaths(changes map[string]any) []string {
	out := make([]string, 0, len(changes))
	for p := range changes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// withChanges adds "changes" (and the sorted "changed" list) to an update's audit detail; nothing changed = "changes": {}.
func withChanges(detail map[string]any, changes map[string]any) map[string]any {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["changes"] = changes
	detail["changed"] = changedPaths(changes)
	return detail
}
