package authz

import (
	"strings"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func operator(tenant string) *arkwenv1.Principal {
	return &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR, PrincipalId: "op-1", TenantId: tenant}
}

// F3: a principal without PERMISSION_GATES_RESOLVE is DENIED (least-privilege),
// and the denial is recorded ONLY in the audit ledger (no run-stream event here).
func TestDefaultDenyMissingGrant(t *testing.T) {
	e := New(nil)
	p := operator("acme")
	e.AddGrant(Grant{PrincipalID: "op-1", Tenant: "acme", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: Selector{Kind: SelectorTenant, Value: "acme"}})

	if got := e.Authorize(p, arkwenv1.Permission_PERMISSION_GATES_RESOLVE, "acme", RunAttrs{RunID: "run-1"}); got != arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY {
		t.Fatalf("gates:resolve without grant should DENY, got %v", got)
	}
	// read is allowed (proves it's not a blanket deny)
	if got := e.Authorize(p, arkwenv1.Permission_PERMISSION_RUNS_READ, "acme", RunAttrs{RunID: "run-1"}); got != arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_ALLOW {
		t.Fatalf("runs:read with grant should ALLOW, got %v", got)
	}
	entries := e.Ledger().Read(0)
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d", len(entries))
	}
	if entries[0].GetAuthDecision().GetOutcome() != arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY {
		t.Fatalf("first entry should be the DENY")
	}
}

// F3: read != write — a read grant never implies gates:resolve or set_floor.
func TestReadIsNotWrite(t *testing.T) {
	e := New(nil)
	p := operator("acme")
	e.AddGrant(Grant{PrincipalID: "op-1", Tenant: "acme", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: Selector{Kind: SelectorTenant, Value: "acme"}})
	for _, perm := range []arkwenv1.Permission{
		arkwenv1.Permission_PERMISSION_GATES_RESOLVE,
		arkwenv1.Permission_PERMISSION_POLICY_SET_FLOOR,
		arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE,
	} {
		if e.Allowed(p, perm, "acme", RunAttrs{RunID: "r"}) {
			t.Fatalf("read grant must not imply %v", perm)
		}
	}
}

// F6: cross-tenant access is default-deny.
func TestCrossTenantDefaultDeny(t *testing.T) {
	e := New(nil)
	alpha := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OUTER_LOOP_CONSUMER, PrincipalId: "consumer-alpha", TenantId: "alpha"}
	e.AddGrant(Grant{PrincipalID: "consumer-alpha", Tenant: "alpha", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: Selector{Kind: SelectorTenant, Value: "alpha"}})

	if got := e.Authorize(alpha, arkwenv1.Permission_PERMISSION_RUNS_READ, "beta", RunAttrs{RunID: "run-fc-c"}); got != arkwenv1.AuthzOutcome_AUTHZ_OUTCOME_DENY {
		t.Fatalf("cross-tenant read should DENY, got %v", got)
	}
	// same-tenant read is fine
	if !e.Allowed(alpha, arkwenv1.Permission_PERMISSION_RUNS_READ, "alpha", RunAttrs{RunID: "r"}) {
		t.Fatalf("same-tenant read should ALLOW")
	}
	last := e.Ledger().Read(0)[0]
	if !strings.Contains(last.GetAuthDecision().GetReason(), "cross-tenant") {
		t.Fatalf("cross-tenant denial reason expected, got %q", last.GetAuthDecision().GetReason())
	}
}

// F7: cross-tenant co-residency forces isolation_profile >= STRICT.
func TestCoResidencyFloor(t *testing.T) {
	if got := CoResidencyFloor("alpha", "beta"); got != arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT {
		t.Fatalf("cross-tenant co-residency must force STRICT, got %v", got)
	}
	if got := CoResidencyFloor("acme", "acme"); got != arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD {
		t.Fatalf("same-tenant floor should be STANDARD, got %v", got)
	}
}

// Intra-trust principals are never external AuthZ subjects (fail-closed).
func TestIntraTrustNeverAuthorized(t *testing.T) {
	e := New(nil)
	shim := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_CELL_SHIM, PrincipalId: "shim", TenantId: "acme"}
	e.AddGrant(Grant{PrincipalID: "shim", Tenant: "acme", Permission: arkwenv1.Permission_PERMISSION_RUNS_READ, Selector: Selector{Kind: SelectorTenant, Value: "acme"}})
	if e.Allowed(shim, arkwenv1.Permission_PERMISSION_RUNS_READ, "acme", RunAttrs{}) {
		t.Fatalf("intra-trust principal must never be authorized as an external subject")
	}
}

// R4 + Invariant 5: structural exclusion — no credential field on the AuthZ types,
// and a bearer token never lands in the audit ledger.
func TestStructuralSecretExclusion(t *testing.T) {
	forbidden := []string{"token", "secret", "credential", "key", "password", "bearer"}
	check := func(name string, fields protoreflect.FieldDescriptors) {
		for i := 0; i < fields.Len(); i++ {
			fn := strings.ToLower(string(fields.Get(i).Name()))
			for _, bad := range forbidden {
				if strings.Contains(fn, bad) {
					t.Fatalf("%s has a field %q that could hold a credential (structural exclusion violated)", name, fn)
				}
			}
		}
	}
	check("Principal", (&arkwenv1.Principal{}).ProtoReflect().Descriptor().Fields())
	check("AuthDecision", (&arkwenv1.AuthDecision{}).ProtoReflect().Descriptor().Fields())
	check("ControlPlaneAuditLedgerEnvelope", (&arkwenv1.ControlPlaneAuditLedgerEnvelope{}).ProtoReflect().Descriptor().Fields())

	// verify-and-discard: authenticate with a bearer token, record a decision,
	// then confirm the token appears nowhere in the marshaled ledger.
	const bearer = "Bearer eyJhbGciOiJFUzI1NiJ9.SECRET_SESSION_TOKEN_do_not_log"
	auth := NewTokenAuthenticator()
	p := &arkwenv1.Principal{Type: arkwenv1.PrincipalType_PRINCIPAL_TYPE_OUTER_LOOP_CONSUMER, PrincipalId: "consumer-01", TenantId: "acme"}
	auth.Bind(bearer, p)
	got, err := auth.Authenticate(bearer)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	e := New(nil)
	e.AddGrant(Grant{PrincipalID: "consumer-01", Tenant: "acme", Permission: arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE, Selector: Selector{Kind: SelectorTenant, Value: "acme"}})
	e.Authorize(got, arkwenv1.Permission_PERMISSION_RUNS_ENQUEUE, "acme", RunAttrs{RunID: "run-1"})
	for _, entry := range e.Ledger().Read(0) {
		b, _ := protojson.Marshal(entry)
		if strings.Contains(string(b), "SECRET_SESSION_TOKEN") {
			t.Fatalf("bearer token leaked into audit ledger: %s", b)
		}
	}
}

// authz_policy_version is deterministic for an unchanged grant-set.
func TestPolicyVersionDeterministic(t *testing.T) {
	e, _ := NewStandalone()
	a := e.PolicyVersion(DefaultTenant)
	b := e.PolicyVersion(DefaultTenant)
	if a.GetHex() != b.GetHex() {
		t.Fatalf("authz_policy_version must be deterministic: %s != %s", a.GetHex(), b.GetHex())
	}
	if a.GetAlgorithm() != arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 {
		t.Fatalf("authz_policy_version must be sha256")
	}
}

// Standalone mode grants the Operator full control within the default tenant.
func TestStandalone(t *testing.T) {
	e, op := NewStandalone()
	for _, perm := range allPermissions() {
		if !e.Allowed(op, perm, DefaultTenant, RunAttrs{RunID: "r"}) {
			t.Fatalf("standalone operator should hold %v", perm)
		}
	}
}
