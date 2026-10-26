package engine

import (
	"sort"
	"strings"
	"text/template/parse"

	"idlifecycle/internal/model"
)

// TemplateFields returns the source fields a template reads, sorted: {{ .Name }}, {{ $.Name }} and
// {{ index . "dotted.key" }}. dynamic is true when a field name is computed at run time
// ({{ index . .Other }}), so the list may be incomplete. Inside {{ with }} the dot is no longer the
// record, so only $-rooted references there count.
func TemplateFields(src string) (fields []string, dynamic bool, err error) {
	t, err := parseTemplate(src)
	if err != nil {
		return nil, false, err
	}
	w := fieldWalker{seen: map[string]bool{}}
	if t.Tree != nil && t.Tree.Root != nil {
		w.node(t.Tree.Root, true)
	}
	out := make([]string, 0, len(w.seen))
	for f := range w.seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, w.dynamic, nil
}

type fieldWalker struct {
	seen    map[string]bool
	dynamic bool
}

// node walks n; dotIsRecord is false inside {{ with }} bodies.
func (w *fieldWalker) node(n parse.Node, dotIsRecord bool) {
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return
		}
		for _, c := range x.Nodes {
			w.node(c, dotIsRecord)
		}
	case *parse.ActionNode:
		w.node(x.Pipe, dotIsRecord)
	case *parse.PipeNode:
		if x == nil {
			return
		}
		for _, c := range x.Cmds {
			w.node(c, dotIsRecord)
		}
	case *parse.CommandNode:
		if key, ok := indexKey(x, dotIsRecord); ok {
			if key == "" {
				w.dynamic = true
			} else {
				w.seen[key] = true
			}
		}
		for _, a := range x.Args {
			w.node(a, dotIsRecord)
		}
	case *parse.FieldNode:
		if dotIsRecord && len(x.Ident) > 0 {
			w.seen[x.Ident[0]] = true
		}
	case *parse.VariableNode:
		if len(x.Ident) > 1 && x.Ident[0] == "$" {
			w.seen[x.Ident[1]] = true
		}
	case *parse.ChainNode:
		w.node(x.Node, dotIsRecord)
	case *parse.IfNode:
		w.node(x.Pipe, dotIsRecord)
		w.node(x.List, dotIsRecord)
		w.node(x.ElseList, dotIsRecord)
	case *parse.WithNode:
		w.node(x.Pipe, dotIsRecord)
		w.node(x.List, false)
		w.node(x.ElseList, dotIsRecord)
	}
}

// indexKey recognises `index <record> <key>`. ok is false for any other command; key is "" when the
// key is not a string literal.
func indexKey(c *parse.CommandNode, dotIsRecord bool) (key string, ok bool) {
	if len(c.Args) < 3 {
		return "", false
	}
	if id, isID := c.Args[0].(*parse.IdentifierNode); !isID || id.Ident != "index" {
		return "", false
	}
	switch r := c.Args[1].(type) {
	case *parse.DotNode:
		if !dotIsRecord {
			return "", false
		}
	case *parse.VariableNode:
		if len(r.Ident) != 1 || r.Ident[0] != "$" {
			return "", false
		}
	default:
		return "", false
	}
	if s, isStr := c.Args[2].(*parse.StringNode); isStr {
		return s.Text, true
	}
	return "", true
}

// FieldRef is one mapping or role rule and the source fields it reads.
type FieldRef struct {
	Kind       string   `json:"kind"` // mapping | role
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	TargetAttr string   `json:"target_attr,omitempty"`
	Enabled    bool     `json:"enabled"`
	Fields     []string `json:"fields"`
	Dynamic    bool     `json:"dynamic"`
	Error      string   `json:"error,omitempty"`
}

// FieldReferences lists, for every mapping and role rule, the source fields it reads (role rules: their
// conditions plus the templates of their attribute grants).
func FieldReferences(mappings []model.Mapping, roles []model.RoleRule) []FieldRef {
	out := make([]FieldRef, 0, len(mappings)+len(roles))
	for _, m := range mappings {
		ref := FieldRef{Kind: "mapping", ID: m.ID, Name: m.Name, TargetAttr: m.TargetAttr, Enabled: m.Enabled, Fields: []string{}}
		f, dyn, err := TemplateFields(m.Template)
		if err != nil {
			ref.Error = strings.TrimPrefix(err.Error(), "template: ")
		}
		ref.Fields, ref.Dynamic = orEmpty(f), dyn
		out = append(out, ref)
	}
	for _, r := range roles {
		ref := FieldRef{Kind: "role", ID: r.ID, Name: r.Name, Enabled: r.Enabled}
		seen := map[string]bool{}
		for _, c := range r.Conditions {
			if a := strings.TrimSpace(c.Attr); a != "" {
				seen[a] = true
			}
		}
		for _, g := range r.Grants.Attributes {
			f, dyn, err := TemplateFields(g.Template)
			if err != nil && ref.Error == "" {
				ref.Error = g.TargetAttr + ": " + strings.TrimPrefix(err.Error(), "template: ")
			}
			ref.Dynamic = ref.Dynamic || dyn
			for _, x := range f {
				seen[x] = true
			}
		}
		for x := range seen {
			ref.Fields = append(ref.Fields, x)
		}
		sort.Strings(ref.Fields)
		ref.Fields = orEmpty(ref.Fields)
		out = append(out, ref)
	}
	return out
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
