// Package app wires the Arkwen runtime together into a ready-to-drive Controller.
// It is the single composition root; every backend it selects is the WSL2-capable
// default behind the same interface a production backend plugs into.
package app

import (
	"context"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/adapter"
	"github.com/arkwen/arkwen/internal/authz"
	"github.com/arkwen/arkwen/internal/cas"
	"github.com/arkwen/arkwen/internal/config"
	"github.com/arkwen/arkwen/internal/controller"
	"github.com/arkwen/arkwen/internal/eventlog"
	"github.com/arkwen/arkwen/internal/isolation"
	"github.com/arkwen/arkwen/internal/secret"
	"github.com/arkwen/arkwen/internal/shim"
	"github.com/arkwen/arkwen/internal/warehouse"
)

// OperatorToken is the standalone-mode credential bound to the Operator
// principal. Verified-and-discarded at the AuthN boundary (never persisted).
const OperatorToken = "operator-token"

// DemoModelAPIKey is the REAL secret injected into every walking-skeleton run so
// the no-secret property test (Invariant 5) protects a real value from S0, not an
// empty set. The worker prints it; the shim MUST redact it before persistence.
const DemoModelAPIKey = "sk-live-ARKWEN51H8Qe_MODEL_API_KEY_do_not_log"

// Runtime bundles the wired components.
type Runtime struct {
	Log        eventlog.Log
	CAS        cas.Store
	Broker     secret.Broker
	IsoReg     *isolation.Registry
	Workers    *adapter.Registry
	Shim       *shim.Shim
	Controller *controller.Controller
	Warehouse  *warehouse.Warehouse
	Authz      *authz.Engine
	Authn      authz.Authenticator
	Operator   *arkwenv1.Principal
}

// New builds an in-memory runtime (default) with the standalone Operator bound.
// Used by the CLI, demo, and the whole test/conformance suite. A durable
// deployment goes through NewFromConfig, which swaps the Log for the PostgreSQL
// backend behind the same interface — no other code changes.
func New() *Runtime {
	return build(eventlog.NewMem(), cas.NewMem(), secret.NewMem(demoResolver(DemoModelAPIKey)), OperatorToken)
}

// NewFromConfig builds the runtime from a resolved config: PostgreSQL event store
// when DATABASE_URL is set (else in-memory), the demo broker seeded with the
// configured model key, and the Operator credential bound ONLY when the token
// mode is non-sealed. It returns a cleanup that releases any durable resources
// (the Postgres pool + LISTEN connection); callers MUST defer it.
func NewFromConfig(ctx context.Context, cfg *config.Config) (*Runtime, func(), error) {
	var log eventlog.Log
	cleanup := func() {}
	if cfg.DatabaseURL != "" {
		l, closer, err := eventlog.NewPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("app: postgres event store: %w", err)
		}
		log, cleanup = l, closer
	} else {
		log = eventlog.NewMem()
	}

	modelKey := cfg.ModelAPIKey
	if modelKey == "" {
		modelKey = DemoModelAPIKey
	}

	// Map the resolved token posture onto a concrete credential. Sealed => bind
	// nothing => every command-plane RPC is Unauthenticated (fail-closed).
	var operatorToken string
	switch cfg.TokenMode {
	case config.TokenProvisioned:
		operatorToken = cfg.OperatorToken
	case config.TokenDevFallback:
		operatorToken = OperatorToken
	case config.TokenSealed:
		operatorToken = ""
	}

	return build(log, cas.NewMem(), secret.NewMem(demoResolver(modelKey)), operatorToken), cleanup, nil
}

// build wires the components. When operatorToken is empty the Operator credential
// is NOT bound — the command plane is sealed and the token authenticator's table
// is empty, so every Authenticate is Unauthenticated (the fail-closed seal path).
func build(log eventlog.Log, store cas.Store, broker secret.Broker, operatorToken string) *Runtime {
	isoReg := isolation.NewRegistry()
	workers := adapter.NewRegistry()
	emit := func(ctx context.Context, ev *arkwenv1.EventEnvelope) error {
		_, err := log.Append(ctx, ev)
		return err
	}
	sh := shim.New(store, broker, isoReg, workers, emit)
	wh := warehouse.New(store)
	ctrl := controller.New(controller.Deps{Log: log, CAS: store, Broker: broker, IsoReg: isoReg, Workers: workers, Warehouse: wh}, sh)

	// Standalone AuthZ: an Operator principal + default single tenant (Invariant 10).
	eng, operator := authz.NewStandalone()
	authn := authz.NewTokenAuthenticator()
	if operatorToken != "" {
		authn.Bind(operatorToken, operator)
	}

	return &Runtime{
		Log: log, CAS: store, Broker: broker, IsoReg: isoReg, Workers: workers,
		Shim: sh, Controller: ctrl, Warehouse: wh, Authz: eng, Authn: authn, Operator: operator,
	}
}

// EnqueueOpts builds controller.EnqueueOptions for the CLI/demo.
func EnqueueOpts(workerKind, workbenchDir, tenant string) controller.EnqueueOptions {
	return controller.EnqueueOptions{
		WorkerKind:   workerKind,
		WorkbenchDir: workbenchDir,
		CreatedBy: &arkwenv1.Principal{
			Type:        arkwenv1.PrincipalType_PRINCIPAL_TYPE_OPERATOR,
			PrincipalId: "cli",
			TenantId:    tenant,
		},
	}
}

// demoResolver injects the given model-API credential for MODEL_API_KEY. The
// default value (DemoModelAPIKey) is the redaction canary the no-secret
// conformance test depends on; a deployment may override it with a real key.
func demoResolver(modelKey string) secret.Option {
	return secret.WithResolver(func(tenant, name string) (string, bool) {
		if name == "MODEL_API_KEY" {
			return modelKey, true
		}
		return "", false
	})
}
