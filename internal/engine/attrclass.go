package engine

import (
	"strings"

	"idlifecycle/internal/ad"
)

// ClassifyAttribute says whether a schema attribute can be the target of a mapping or role grant, and
// why not. hint is advice for a mappable attribute (e.g. DN-valued ones need value type user_ref).
// It applies the same rules as ValidateMapping plus what the schema tells us; the scripts write
// single-valued text with -Replace, so anything else is refused.
func ClassifyAttribute(a ad.SchemaAttribute) (mappable bool, reason, hint string) {
	switch {
	case !attrNameRe.MatchString(a.Name):
		return false, "not a valid LDAP attribute name for IDLifecycle", ""
	case IsProtectedAttribute(a.Name):
		return false, "managed by IDLifecycle or security-sensitive", ""
	case a.Constructed:
		return false, "computed by AD", ""
	case a.BackLink():
		return false, "back-link maintained by AD", ""
	case a.SystemOnly:
		return false, "only AD itself can write it", ""
	case !a.SingleValued:
		return false, "multi-valued", ""
	}
	switch a.Syntax {
	case "string", "integer", "boolean", "time":
	case "dn":
		hint = "holds a reference to another object: use value type user_ref"
	default:
		return false, "not a text value (" + a.Syntax + ")", ""
	}
	if strings.EqualFold(a.Name, "employeeID") {
		hint = "the default link key: always treated as immutable"
	}
	return true, "", hint
}
