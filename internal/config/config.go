// Package config resolves the runtime configuration and the one security posture
// decision from the environment. Everything the deployment needs to know — where
// to bind, which event store to use, and whether the command plane is open — is
// derived here, exactly once, so the fail-closed decision (Invariant 7) lives in
// a single, unit-testable place.
//
// This package deliberately imports nothing from internal/app: the default
// credential + model-key literals live in app (the composition root owns them),
// and app maps a resolved TokenMode onto those literals. That keeps the
// dependency edge one-way (app -> config) and avoids duplicating secrets.
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// TokenMode records how the standalone operator credential resolved. It drives
// the startup log line and whether the command plane is usable at all. The value
// is NOT a secret — it names a posture, never a credential.
type TokenMode int

const (
	// TokenProvisioned: ARKWEN_OPERATOR_TOKEN was supplied — bind exactly that
	// token. The only mode that ever carries a stable, shareable credential.
	TokenProvisioned TokenMode = iota
	// TokenDevFallback: token unset on a loopback bind — app binds its compiled-in
	// dev constant. Never reachable from the network, so it is safe here and keeps
	// `arkwen ctl` zero-config locally.
	TokenDevFallback
	// TokenSealed: token unset on a PUBLIC bind — bind NO credential. The command
	// plane is closed (every RPC is Unauthenticated) until an operator provisions a
	// token. Strictly fail-closed; holds zero secret material (Invariant 7).
	TokenSealed
)

func (m TokenMode) String() string {
	switch m {
	case TokenProvisioned:
		return "provisioned"
	case TokenDevFallback:
		return "dev-fallback"
	case TokenSealed:
		return "sealed"
	default:
		return "unknown"
	}
}

// Config is the fully-resolved runtime configuration. Secret-bearing fields
// (OperatorToken, ModelAPIKey) are never rendered by String/LogFields.
type Config struct {
	BindAddr string // host:port; always explicit
	Public   bool   // true when bound to a non-loopback address (e.g. Railway)

	DatabaseURL   string // Postgres DSN; "" => in-memory Log (graceful degradation, Inv. 9)
	DefaultWorker string
	AutoDrive     bool

	// OperatorToken is the env-provided command-plane credential. It is populated
	// ONLY in TokenProvisioned mode; in dev-fallback/sealed modes it is empty and
	// the composition root decides what (if anything) to bind. NEVER logged.
	OperatorToken string
	TokenMode     TokenMode

	// ModelAPIKey overrides the demo broker credential. "" => the composition root
	// substitutes its compiled-in redaction canary. NEVER logged.
	ModelAPIKey string

	// RequireToken makes an unset ARKWEN_OPERATOR_TOKEN a fatal startup error
	// (opt-in strict production) rather than sealing.
	RequireToken bool

	// AllowInsecurePublic acknowledges that a provisioned token may cross a
	// PLAINTEXT public socket (Railway's TCP proxy does not terminate TLS). Without
	// it, a provisioned token on a public bind is fail-closed (Load errors) rather
	// than silently putting a live credential on the wire in cleartext (Invariant 7).
	AllowInsecurePublic bool
}

// LogFields returns ONLY non-secret fields, for a safe structured startup line.
// There is deliberately no String()/Stringer that could dump a credential.
func (c *Config) LogFields() map[string]any {
	return map[string]any{
		"bind":      c.BindAddr,
		"public":    c.Public,
		"store":     StoreName(c.DatabaseURL),
		"worker":    c.DefaultWorker,
		"autodrive": c.AutoDrive,
		"token":     c.TokenMode.String(),
	}
}

// StoreName names the event-store backend for logging (never the DSN itself).
func StoreName(dsn string) string {
	if dsn != "" {
		return "postgres"
	}
	return "in-memory"
}

// Load resolves the configuration from getenv (pass os.Getenv in production; a
// fake map in tests). addrOverride is the CLI --addr flag ("" if unset): it is
// folded into the bind BEFORE the security decision, so the fail-closed posture
// is always derived from the ADDRESS ACTUALLY BOUND, never a stale env default.
// Load performs the single fail-closed security decision.
func Load(getenv func(string) string, addrOverride string) (*Config, error) {
	c := &Config{
		DatabaseURL:         getenv("DATABASE_URL"),
		DefaultWorker:       firstNonEmpty(getenv("ARKWEN_DEFAULT_WORKER"), "claude-code"),
		ModelAPIKey:         getenv("ARKWEN_MODEL_API_KEY"), // "" => app default (canary)
		RequireToken:        parseBool(getenv("ARKWEN_REQUIRE_OPERATOR_TOKEN"), false),
		AutoDrive:           parseBool(getenv("ARKWEN_AUTODRIVE"), true),
		AllowInsecurePublic: parseBool(getenv("ARKWEN_ALLOW_INSECURE_PUBLIC"), false),
	}

	// --- bind address --------------------------------------------------------
	// Railway injects PORT. When it is present we are containerised and must bind
	// all interfaces; we bind "::" so ONE dual-stack socket serves both the public
	// IPv4 TCP-proxy path AND the IPv6-only private network (Go opens "[::]" with
	// v6only=0). Local dev (no PORT) keeps loopback — behaviour unchanged.
	//
	// A --addr override wins and re-derives the host, so the seal decision below
	// runs on the effective bind (e.g. `--addr :8080` / `0.0.0.0:8080` => Public).
	var host string
	if addrOverride != "" {
		h, p, err := net.SplitHostPort(addrOverride)
		if err != nil {
			return nil, fmt.Errorf("config: --addr %q must be host:port: %w", addrOverride, err)
		}
		if _, err := strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("config: --addr %q has a non-numeric port", addrOverride)
		}
		host, c.BindAddr = h, addrOverride
	} else {
		port := firstNonEmpty(getenv("PORT"), "7777")
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("config: PORT %q is not a valid port", port)
		}
		host = getenv("ARKWEN_BIND_HOST")
		if host == "" {
			if getenv("PORT") != "" {
				host = "::" // containerised / Railway: dual-stack all interfaces
			} else {
				host = "127.0.0.1" // local dev
			}
		}
		c.BindAddr = net.JoinHostPort(host, port)
	}
	// An empty/unspecified host ("" from ":8080", "0.0.0.0", "::") is NOT loopback
	// => public => fail-closed applies.
	c.Public = !isLoopback(host)

	// --- the one security decision (Invariant 7), made once ------------------
	tok := getenv("ARKWEN_OPERATOR_TOKEN")
	switch {
	case tok != "":
		c.OperatorToken, c.TokenMode = tok, TokenProvisioned
	case c.RequireToken:
		return nil, errors.New("config: ARKWEN_OPERATOR_TOKEN is required " +
			"(ARKWEN_REQUIRE_OPERATOR_TOKEN=1) but unset")
	case c.Public:
		// Public bind + unset => SEALED. No credential bound; the command plane is
		// closed to everyone until a token is provisioned. Health + read-only
		// status still serve. Fail-closed, holds zero secret.
		c.OperatorToken, c.TokenMode = "", TokenSealed
	default:
		// Loopback + unset => dev convenience; the composition root binds its
		// compiled-in constant (never reachable from the network).
		c.OperatorToken, c.TokenMode = "", TokenDevFallback
	}

	// A provisioned token on a PUBLIC bind would cross the network (Railway's TCP
	// proxy is plaintext). Refuse rather than silently place a live credential on a
	// cleartext socket — unless the operator explicitly acknowledges the risk. This
	// is the same fail-closed spirit as the seal (Invariant 7); the escape hatch is
	// a single conscious opt-in, not a silent default. (TLS via the grpc.ServerOption
	// seam is the real fix; this guards the plaintext MVP path.)
	if c.TokenMode == TokenProvisioned && c.Public && !c.AllowInsecurePublic {
		return nil, errors.New("config: refusing to bind a provisioned operator token " +
			"on a public plaintext socket; set ARKWEN_ALLOW_INSECURE_PUBLIC=1 to accept " +
			"cleartext-token risk (demo), or terminate TLS in front of the command plane")
	}
	return c, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseBool(s string, def bool) bool {
	if s == "" {
		return def
	}
	if v, err := strconv.ParseBool(s); err == nil {
		return v
	}
	return def
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
