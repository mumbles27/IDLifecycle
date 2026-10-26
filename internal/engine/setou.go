package engine

import (
	"context"
	"fmt"
	"strings"

	"idlifecycle/internal/model"
)

// MaxOUOverride is the longest OU a person can choose for one identity (the column is 255 characters).
const MaxOUOverride = 255

// SetChangeOU puts ou on a pending (or simulated) create, update or enable: the create's OU or the move target. The
// choice is remembered for the person (it applies until the placement field - the department - changes), so the
// next sync plans the same change instead of replacing it. With addRule the OU is also added to the placement rules
// for the person's department value. Returns the updated change.
func (e *Engine) SetChangeOU(ctx context.Context, changeID, ou, by string, addRule bool) (model.Change, error) {
	ou = strings.TrimSpace(ou)
	if !strings.Contains(ou, "=") || strings.ContainsAny(ou, "\r\n") {
		return model.Change{}, fmt.Errorf("%w: %q is not an OU distinguished name (OU=...,DC=...)", ErrInvalid, ou)
	}
	if len([]rune(ou)) > MaxOUOverride {
		return model.Change{}, fmt.Errorf("%w: the OU is longer than %d characters", ErrInvalid, MaxOUOverride)
	}
	c, err := e.Store.GetChange(ctx, changeID)
	if err != nil {
		return c, err
	}
	if c.Status != model.StatusPending && c.Status != model.StatusSimulated {
		return c, fmt.Errorf("%w: the OU can only be changed while the change is pending (it is %s)", ErrInvalid, c.Status)
	}
	id, err := e.Store.GetIdentity(ctx, c.IdentityID)
	if err != nil {
		return c, err
	}
	settings, err := e.Store.GetSettings(ctx)
	if err != nil {
		return c, err
	}
	value := c.Payload.Placement
	if value == "" && settings.Placement.Enabled {
		value = fieldValue(id.Attrs, settings.Placement.Field)
	}

	p := c.Payload
	prev := p.OU
	switch c.Type {
	case model.ChangeCreate:
		p.OU = ou
	case model.ChangeUpdate, model.ChangeEnable:
		prev = p.MoveTo
		if strings.EqualFold(parentDN(p.DN), ou) {
			p.MoveTo = "" // already there
		} else {
			p.MoveTo = ou
		}
		if settings.Placement.Enabled {
			p.Placement = value
		}
	default:
		return c, fmt.Errorf("%w: a %s change has no OU", ErrInvalid, c.Type)
	}
	p.NeedsOU, p.OUReason = false, ""

	summary := make([]string, 0, len(c.Summary)+1)
	for _, line := range c.Summary {
		if strings.HasPrefix(line, "Choose an OU:") || strings.HasPrefix(line, "WAITING for an OU") || strings.HasPrefix(line, "OU chosen by ") {
			continue
		}
		summary = append(summary, line)
	}
	summary = append([]string{fmt.Sprintf("OU chosen by %s: %s", by, ou)}, summary...)

	if err := e.Store.SetIdentityOU(ctx, id.ID, ou, value); err != nil {
		return c, err
	}
	if err := e.Store.UpdateChangePayload(ctx, c.ID, c.Status, p, summary, PayloadHash(c.Type, p)); err != nil {
		return c, err
	}
	ruleAdded := false
	if addRule && value != "" {
		rules := settings.Placement.Rules[:0:0]
		for _, r := range settings.Placement.Rules {
			if !strings.EqualFold(strings.TrimSpace(r.Match), value) {
				rules = append(rules, r)
			}
		}
		settings.Placement.Rules = append(rules, model.PlacementRule{Match: value, OU: ou})
		if err := settings.Validate(); err != nil {
			return c, err
		}
		if err := e.Store.SaveSettings(ctx, settings, by); err != nil {
			return c, err
		}
		ruleAdded = true
	}
	e.audit(ctx, by, "change.set_ou", "change", c.ID, map[string]any{
		"ou": ou, "previous": prev, "external_id": c.ExternalID, "placement_value": value, "rule_added": ruleAdded,
	})
	return e.Store.GetChange(ctx, c.ID)
}
