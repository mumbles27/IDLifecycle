package engine

import (
	"strings"

	"idlifecycle/internal/model"
)

// PreviewAttributes renders the effective attribute set (global mappings overlaid by role grants)
// for a sample record so the GUI can show what a person would receive. user_ref values show the
// raw reference because no directory lookup happens in a preview.
func PreviewAttributes(mappings []model.Mapping, grants []model.AttrGrant, record map[string]string) (map[string]string, map[string]string) {
	out := map[string]string{}
	errs := map[string]string{}
	for _, m := range mergeMappings(mappings, grants) {
		v, err := Render(m.Template, record)
		if err != nil {
			errs[m.Attr] = strings.TrimPrefix(err.Error(), "template: ")
			continue
		}
		out[m.Attr] = v
	}
	return out, errs
}
