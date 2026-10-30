package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// UndoItem is one part of an applied update or cloud_groups change that can be undone on its own. Key is stable:
// attribute:<ldapName>, group_add:<group DN>, group_remove:<group DN>, ou, cloud_add:<object id>, cloud_remove:<object id>.
type UndoItem struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"` // attribute | group_add | group_remove | ou | cloud_add | cloud_remove
	Label string `json:"label"`
	// From / To: the value before and after the change (attribute: nil = empty; ou: the OU; groups: the group).
	From       *string `json:"from"`
	To         *string `json:"to"`
	Selectable bool    `json:"selectable"`
	UndoneBy   string  `json:"undone_by,omitempty"`    // the undo change that rolled this part back
	PendingIn  string  `json:"pending_undo,omitempty"` // an open undo change that covers this part
	Manual     bool    `json:"manual"`                 // a Manual pin (hold) is in force for this part
	HoldID     string  `json:"hold_id,omitempty"`      // that pin
	Group      string  `json:"group,omitempty"`        // group items: the group DN or cloud object id
	Attribute  string  `json:"attribute,omitempty"`    // attribute items: the LDAP name
}

// Item kinds and key prefixes.
const (
	ItemAttribute   = "attribute"
	ItemGroupAdd    = "group_add"
	ItemGroupRemove = "group_remove"
	ItemOU          = "ou"
	ItemCloudAdd    = "cloud_add"
	ItemCloudRemove = "cloud_remove"

	itemAttribute   = ItemAttribute + ":"
	itemGroupAdd    = ItemGroupAdd + ":"
	itemGroupRemove = ItemGroupRemove + ":"
	itemOU          = ItemOU
	itemCloudAdd    = ItemCloudAdd + ":"
	itemCloudRemove = ItemCloudRemove + ":"
)

func strp(s string) *string { return &s }

func quoted(p *string) string {
	if p == nil || *p == "" {
		return "(empty)"
	}
	return "'" + *p + "'"
}

// itemsOf lists the parts of change c (of planning type typ: an undo change uses its script's type). Only update and
// cloud_groups changes have parts; the rest return nil. Parts already undone or covered by an open undo are marked.
func itemsOf(c model.Change, typ string) []UndoItem {
	st := c.AppliedState
	if st == nil {
		return nil
	}
	b := st.Before
	var out []UndoItem
	switch typ {
	case model.ChangeUpdate:
		for _, k := range sortedKeys(b.Attributes) {
			was := b.Attributes[k]
			now, known := lookupFold(st.Wrote, k)
			if known && valueOf(was) == valueOf(now) && (was == nil) == (now == nil) {
				continue
			}
			to := now
			if !known {
				to = nil
			}
			out = append(out, UndoItem{Key: itemAttribute + k, Kind: ItemAttribute, Attribute: k, From: was, To: to,
				Label: fmt.Sprintf("%s: %s → %s", k, quoted(was), quoted(to))})
		}
		for _, g := range b.GroupsAdded {
			out = append(out, UndoItem{Key: itemGroupAdd + g, Kind: ItemGroupAdd, Group: g, To: strp(g), Label: "Added to group " + shortCN(g)})
		}
		for _, g := range b.GroupsRemoved {
			out = append(out, UndoItem{Key: itemGroupRemove + g, Kind: ItemGroupRemove, Group: g, From: strp(g), Label: "Removed from group " + shortCN(g)})
		}
		if b.DN != "" && st.DN != "" && !strings.EqualFold(parentDN(b.DN), parentDN(st.DN)) {
			out = append(out, UndoItem{Key: itemOU, Kind: ItemOU, From: strp(parentDN(b.DN)), To: strp(parentDN(st.DN)),
				Label: "Moved from " + parentDN(b.DN) + " to " + parentDN(st.DN)})
		}
	case model.ChangeCloudGroups:
		for _, g := range b.CloudAdded {
			out = append(out, UndoItem{Key: itemCloudAdd + g, Kind: ItemCloudAdd, Group: g, To: strp(g), Label: "Added to Entra group " + g})
		}
		for _, g := range b.CloudRemoved {
			out = append(out, UndoItem{Key: itemCloudRemove + g, Kind: ItemCloudRemove, Group: g, From: strp(g), Label: "Removed from Entra group " + g})
		}
	default:
		return nil
	}
	for i := range out {
		it := &out[i]
		switch {
		case c.UndoneBy != "":
			it.UndoneBy = c.UndoneBy
		case c.UndoneItems[it.Key] != "":
			it.UndoneBy = c.UndoneItems[it.Key]
		case c.UndoPending != "":
			it.PendingIn = c.UndoPending
		case c.UndoPendingItems[it.Key] != "":
			it.PendingIn = c.UndoPendingItems[it.Key]
		}
		it.Selectable = it.UndoneBy == "" && it.PendingIn == ""
	}
	return out
}

// holdKindOf maps a part to the hold (Manual pin) that keeps the planner from redoing it.
func holdKindOf(it UndoItem) (kind, key string) {
	switch it.Kind {
	case ItemAttribute:
		return model.HoldAttribute, it.Attribute
	case ItemGroupAdd, ItemGroupRemove:
		return model.HoldGroup, it.Group
	case ItemOU:
		return model.HoldOU, ""
	case ItemCloudAdd, ItemCloudRemove:
		return model.HoldCloudGroup, it.Group
	}
	return "", ""
}

// markManual flags the parts that a hold in force at now already pins.
func markManual(items []UndoItem, holds []model.Hold, now time.Time) {
	for i := range items {
		kind, key := holdKindOf(items[i])
		for _, h := range holds {
			if h.Kind == kind && h.Active(now) && (kind == model.HoldOU || strings.EqualFold(h.Key, key)) {
				items[i].Manual, items[i].HoldID = true, h.ID
				break
			}
		}
	}
}

// selectItems resolves which parts an undo covers. want empty = every part still selectable. whole reports that the
// selection is the entire change (then the undo is recorded as a whole-change undo).
func selectItems(items []UndoItem, selectable bool, want []string) (sel map[string]bool, whole bool, err error) {
	if !selectable {
		if len(want) > 0 {
			return nil, true, fmt.Errorf("%w: this change can only be undone as a whole; parts cannot be chosen", ErrInvalid)
		}
		return nil, true, nil
	}
	byKey := make(map[string]UndoItem, len(items))
	for _, it := range items {
		byKey[it.Key] = it
	}
	sel = map[string]bool{}
	if len(want) == 0 {
		for _, it := range items {
			if it.Selectable {
				sel[it.Key] = true
			}
		}
	}
	for _, k := range want {
		k = strings.TrimSpace(k)
		it, ok := byKey[k]
		switch {
		case k == "":
			continue
		case !ok:
			return nil, false, fmt.Errorf("%w: %q is not a part of this change", ErrInvalid, k)
		case it.UndoneBy != "":
			return nil, false, fmt.Errorf("%w: %s was already undone (change %s)", ErrInvalid, it.Label, it.UndoneBy)
		case it.PendingIn != "":
			return nil, false, fmt.Errorf("%w: %s is already in an undo waiting for approval (change %s)", store.ErrConflict, it.Label, it.PendingIn)
		}
		sel[k] = true
	}
	return sel, len(items) > 0 && len(sel) == len(items), nil
}

// overlaps reports whether two undos of the same change cover a common part (no items = the whole change).
func overlaps(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	set := make(map[string]bool, len(a))
	for _, k := range a {
		set[k] = true
	}
	for _, k := range b {
		if set[k] {
			return true
		}
	}
	return false
}

// markUndone records that undo change undoID rolled back parts of change of (none = all of it). The change counts as
// undone (UndoneBy) once every part is.
func (e *Engine) markUndone(ctx context.Context, of, undoID string, items []string) {
	if len(items) > 0 {
		if err := e.Store.AddUndoneItems(ctx, of, undoID, items); err != nil {
			e.Log.Error("mark parts undone", "change", of, "undo", undoID, "error", err)
			return
		}
		orig, err := e.Store.GetChange(ctx, of)
		if err != nil {
			e.Log.Error("load change after a partial undo", "change", of, "error", err)
			return
		}
		typ := orig.Type
		if orig.Type == model.ChangeUndo && orig.Payload.Undo != nil {
			typ = scriptType(orig.Payload.Undo.Script)
		}
		for _, it := range itemsOf(orig, typ) {
			if it.UndoneBy == "" {
				return // parts are left
			}
		}
	}
	if err := e.Store.SetChangeUndoneBy(ctx, of, undoID); err != nil {
		e.Log.Error("mark change undone", "change", of, "undo", undoID, "error", err)
	}
}
