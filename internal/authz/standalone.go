package authz

import arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"

// DefaultTenant is the single tenant used in standalone mode (Invariant 10: no
// consumer name anywhere; Arkwen runs perfectly well with no outer loop).
const DefaultTenant = "default"

// allPermissions is the full least-privilege permission set (excluding the
// UNSPECIFIED sentinel). Standalone grants all of them to the Operator within the
// default tenant; a real multi-tenant deployment grants a strict subset.
func allPermissions() []arkwenv1.Permission {
	return []arkwenv1.Permission{
		arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE,
		arkwenv1.Permission_PERMISSION_RUNS_READ,
		arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_CANCEL,
		arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_PAUSE,
		arkwenv1.Permission_PERMISSION_RUNS_SIGNAL_RESUME,
		arkwenv1.Permission_PERMISSION_GATES_RESOLVE,
		arkwenv1.Permission_PERMISSION_RUNS_REPRIORITIZE,
		arkwenv1.Permission_PERMISSION_POLICY_SET_FLOOR,
		arkwenv1.Permission_PERMISSION_WAREHOUSE_PROMOTE,
	}
}

// NewStandalone builds a fully-configured single-tenant Engine plus the Operator
// principal that owns it (ADR-010: standalone mode = Operator-principal +
// default single tenant, minimally configured). Multi-outer-loop deployments
// instead create per-tenant principals with least-privilege subsets — tenancy is
// then the isolation boundary (loop A cannot touch loop B's runs).
func NewStandalone() (*Engine, *arkwenv1.Principal) {
	e := New(nil)
	op := &arkwenv1.Principal{
		Type:        arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR,
		PrincipalId: "operator",
		TenantId:    DefaultTenant,
	}
	for _, p := range allPermissions() {
		e.AddGrant(Grant{
			PrincipalID: op.GetPrincipalId(),
			Tenant:      DefaultTenant,
			Permission:  p,
			Selector:    Selector{Kind: SelectorTenant, Value: DefaultTenant},
		})
	}
	return e, op
}
