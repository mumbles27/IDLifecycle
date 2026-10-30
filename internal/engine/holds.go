package engine

import (
	"context"
	"strings"

	"idlifecycle/internal/model"
)

// lifecycleChange reports whether a change type moves an account through its lifecycle (what a state hold stops).
func lifecycleChange(typ string) bool {
	switch typ {
	case model.ChangeCreate, model.ChangeLink, model.ChangeDisable, model.ChangeEnable, model.ChangeDelete:
		return true
	}
	return false
}

// holdFor returns the active hold of identityID that covers kind/key (ou, state and all holds have no key), or nil.
func (pc *PlanContext) holdFor(identityID, kind, key string) *model.Hold {
	hs := pc.Holds[identityID]
	for i := range hs {
		h := &hs[i]
		if h.Kind != kind || !h.Active(pc.now()) {
			continue
		}
		if kind == model.HoldOU || kind == model.HoldState || kind == model.HoldAll || strings.EqualFold(strings.TrimSpace(h.Key), strings.TrimSpace(key)) {
			return h
		}
	}
	return nil
}

// held reports whether an active hold of identityID covers kind/key.
func (pc *PlanContext) held(identityID, kind, key string) bool {
	return pc.holdFor(identityID, kind, key) != nil
}

// hold is held, counting a suppressed item and remembering what the hold stopped (what, e.g. "would set title to
// 'Lead'"), so the console can say what IDLifecycle would do if the item were set back to Auto.
func (pc *PlanContext) hold(identityID, kind, key, what string) bool {
	h := pc.holdFor(identityID, kind, key)
	if h == nil {
		return false
	}
	pc.OnHold++
	pc.suppress(h.ID, what)
	return true
}

// suppress records what hold holdID kept the planner from doing.
func (pc *PlanContext) suppress(holdID, what string) {
	if what == "" {
		return
	}
	if pc.Suppressed == nil {
		pc.Suppressed = map[string][]string{}
	}
	pc.Suppressed[holdID] = append(pc.Suppressed[holdID], what)
}

// SuppressedText joins what one hold stopped in a run, capped for storage ("" = nothing).
func SuppressedText(lines []string) string {
	const maxLen = 1000
	t := strings.Join(lines, "; ")
	if r := []rune(t); len(r) > maxLen {
		t = string(r[:maxLen-1]) + "…"
	}
	return t
}

// dropHeldMove removes a planned OU move (and an OU choice) from an update when the identity has an ou hold.
func (pc *PlanContext) dropHeldMove(identityID string, p *Planned) {
	if p.Payload.MoveTo == "" && !p.Payload.NeedsOU {
		return
	}
	what := "would move the account to " + p.Payload.MoveTo
	if p.Payload.MoveTo == "" {
		what = "would ask for an OU (" + p.Payload.OUReason + ")"
	}
	if !pc.hold(identityID, model.HoldOU, "", what) {
		return
	}
	p.Payload.MoveTo, p.Payload.NeedsOU, p.Payload.OUReason, p.Payload.Placement = "", false, "", ""
	kept := p.Summary[:0]
	for _, l := range p.Summary {
		if !strings.HasPrefix(l, "Move ") && !strings.HasPrefix(l, "Choose an OU") {
			kept = append(kept, l)
		}
	}
	p.Summary = kept
}

// dropHeldCloud removes cloud group ids that the identity has cloud_group holds for. verb says what the plan would
// have done ("add to" / "remove from"); label names a group for people.
func dropHeldCloud(pc *PlanContext, identityID string, ids []string, verb string, label func(string) string) []string {
	var out []string
	for _, g := range ids {
		if pc.hold(identityID, model.HoldCloudGroup, g, "would "+verb+" Entra group "+label(g)) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// heldWhole reports whether a person-level hold stops plan p for identity id: an `all` hold stops everything, a
// `state` hold stops lifecycle changes. It records what was stopped, and an inactive person whose disable or delete
// was stopped is remembered in ManualLeft (they left the source but keep an enabled account).
func (pc *PlanContext) heldWhole(id model.Identity, p *Planned) bool {
	if p == nil || p.Type == "" {
		return false
	}
	what := wouldText(p)
	h := pc.holdFor(id.ID, model.HoldAll, "")
	if h == nil && lifecycleChange(p.Type) {
		h = pc.holdFor(id.ID, model.HoldState, "")
	}
	if h == nil {
		return false
	}
	pc.OnHold++
	pc.suppress(h.ID, what)
	if !id.Active && (p.Type == model.ChangeDisable || p.Type == model.ChangeDelete) {
		pc.ManualLeft = append(pc.ManualLeft, id.Display+" ("+id.ExternalID+")")
	}
	return true
}

// wouldText says in a few words what a plan would do (for a Manual pin's "IDLifecycle would ...").
func wouldText(p *Planned) string {
	switch p.Type {
	case model.ChangeCreate:
		return "would create an account"
	case model.ChangeLink:
		return "would link an existing account"
	case model.ChangeDisable:
		return "would disable the account"
	case model.ChangeEnable:
		return "would enable the account"
	case model.ChangeDelete:
		return "would delete the account"
	case model.ChangeCloudGroups:
		return "would change Entra / Microsoft 365 groups: " + strings.Join(capLines(p.Summary, 3), "; ")
	}
	return "would " + strings.ToLower(strings.Join(capLines(p.Summary, 3), "; "))
}

func capLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	out := append([]string{}, lines[:n]...)
	return append(out, "…")
}

// recordSuppressed stores, for every hold in force, what this sync would have done without it (only when that text
// changed, so an unchanged run writes nothing). The impact preview never calls this.
func (e *Engine) recordSuppressed(ctx context.Context, holds map[string][]model.Hold, got map[string][]string) {
	for _, hs := range holds {
		for _, h := range hs {
			text := SuppressedText(got[h.ID])
			old := ""
			if h.Suppressed != nil {
				old = h.Suppressed.Summary
			}
			if text == old {
				continue
			}
			if err := e.Store.SetHoldSuppressed(ctx, h.ID, text, e.Now()); err != nil {
				e.Log.Warn("record what a hold stopped", "hold", h.ID, "error", err)
			}
		}
	}
}
