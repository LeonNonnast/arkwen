package controller

import (
	"context"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/warehouse"
)

// warehousePromo fills the Slice-3 Warehouse Intake seam with the Slice-4 gate
// spine at scope=promotion (ADR-007 E5): when a promotion gate resolves, it
// records the resolution in the Warehouse ledger and, on APPROVE, moves the
// target channel to the subject digest. Promotion lives in the ledger domain, NOT
// the run stream (ADR-009 R8) — the disjoint truth domains stay disjoint.
type warehousePromo struct{ wh *warehouse.Warehouse }

func (h *warehousePromo) OnPromotionResolved(ctx context.Context, gateID string, decision arkwenv1.GateDecision, by *arkwenv1.Principal) error {
	for _, e := range h.wh.Ledger().Read(0) {
		ir := e.GetIntakeRequested()
		if ir == nil || ir.GetGate().GetGateId() != gateID {
			continue
		}
		h.wh.Ledger().RecordIntakeResolved(by, gateID, decision, ir.GetSubject(), "")
		if decision == arkwenv1.GateDecision_GATE_DECISION_APPROVE {
			if _, err := h.wh.MoveChannel(ir.GetChannel(), ir.GetSubject(), by); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("controller: no pending warehouse intake for gate %s", gateID)
}
