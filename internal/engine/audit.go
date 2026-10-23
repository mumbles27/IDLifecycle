package engine

import "context"

func (e *Engine) audit(ctx context.Context, actor, action, entityType, entityID string, detail map[string]any) {
	if err := e.Store.AppendAudit(ctx, actor, action, entityType, entityID, detail); err != nil {
		e.Log.Error("audit append failed", "action", action, "entity", entityID, "error", err)
	}
}
