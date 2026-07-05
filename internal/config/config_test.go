package config

import "testing"

// env builds a getenv func over a map for hermetic Load tests.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// The security decision is the crux: on a PUBLIC bind with no token, the command
// plane MUST seal (bind no credential) — fail-closed (Invariant 7).
func TestLoad_PublicUnsetTokenSeals(t *testing.T) {
	c, err := Load(env(map[string]string{"PORT": "8080"}), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenMode != TokenSealed {
		t.Fatalf("public+unset must seal, got %v", c.TokenMode)
	}
	if c.OperatorToken != "" {
		t.Fatal("sealed mode must hold NO token")
	}
	if !c.Public {
		t.Fatal("PORT set => public bind")
	}
	if c.BindAddr != "[::]:8080" {
		t.Fatalf("public bind must be dual-stack [::]:PORT, got %q", c.BindAddr)
	}
}

// Loopback + unset => dev fallback (safe: unreachable from the network).
func TestLoad_LoopbackUnsetDevFallback(t *testing.T) {
	c, err := Load(env(map[string]string{}), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenMode != TokenDevFallback {
		t.Fatalf("loopback+unset must be dev-fallback, got %v", c.TokenMode)
	}
	if c.Public {
		t.Fatal("no PORT => loopback => not public")
	}
	if c.BindAddr != "127.0.0.1:7777" {
		t.Fatalf("dev bind must be loopback, got %q", c.BindAddr)
	}
}

// A provisioned token is bound verbatim; on a public bind it also requires the
// explicit insecure-public acknowledgement.
func TestLoad_ProvisionedToken(t *testing.T) {
	c, err := Load(env(map[string]string{
		"PORT": "8080", "ARKWEN_OPERATOR_TOKEN": "s3cret", "ARKWEN_ALLOW_INSECURE_PUBLIC": "1",
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenMode != TokenProvisioned || c.OperatorToken != "s3cret" {
		t.Fatalf("provisioned token must bind verbatim, got mode=%v tok=%q", c.TokenMode, c.OperatorToken)
	}
}

// RequireToken turns an unset token into a fatal startup error (strict prod).
func TestLoad_RequireTokenFailsClosed(t *testing.T) {
	_, err := Load(env(map[string]string{"PORT": "8080", "ARKWEN_REQUIRE_OPERATOR_TOKEN": "1"}), "")
	if err == nil {
		t.Fatal("require-token with unset token must error")
	}
}

// A bad PORT is rejected (fail-closed on malformed config).
func TestLoad_BadPortRejected(t *testing.T) {
	if _, err := Load(env(map[string]string{"PORT": "not-a-port"}), ""); err == nil {
		t.Fatal("invalid PORT must error")
	}
	if _, err := Load(env(map[string]string{"PORT": "70000"}), ""); err == nil {
		t.Fatal("out-of-range PORT must error")
	}
}

// LogFields must never carry a secret (Invariant 5): no operator token, no model key.
func TestLogFields_NoSecrets(t *testing.T) {
	c, err := Load(env(map[string]string{
		"PORT":                         "8080",
		"ARKWEN_OPERATOR_TOKEN":        "topsecret-token",
		"ARKWEN_MODEL_API_KEY":         "sk-live-REAL",
		"ARKWEN_ALLOW_INSECURE_PUBLIC": "1",
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.LogFields() {
		if s, ok := v.(string); ok {
			if s == "topsecret-token" || s == "sk-live-REAL" {
				t.Fatalf("LogFields leaked a secret in %q", k)
			}
		}
	}
}

// ARKWEN_BIND_HOST overrides the derived host and re-evaluates public/loopback.
func TestLoad_BindHostOverride(t *testing.T) {
	c, err := Load(env(map[string]string{"PORT": "8080", "ARKWEN_BIND_HOST": "127.0.0.1"}), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Public {
		t.Fatal("explicit loopback host must not be public even with PORT set")
	}
	if c.TokenMode != TokenDevFallback {
		t.Fatalf("loopback override => dev-fallback, got %v", c.TokenMode)
	}
}

// REGRESSION (review finding, HIGH): a public --addr override with an unset token
// MUST seal — the security decision follows the address actually bound, not the
// env default. `arkwen serve --addr 0.0.0.0:8080` must not open the dev token.
func TestLoad_PublicAddrOverrideSeals(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		c, err := Load(env(map[string]string{}), addr) // no PORT, no token: env alone => dev-fallback
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if !c.Public {
			t.Fatalf("%s: public --addr must be Public", addr)
		}
		if c.TokenMode != TokenSealed {
			t.Fatalf("%s: public --addr + unset token must SEAL, got %v", addr, c.TokenMode)
		}
		if c.OperatorToken != "" {
			t.Fatalf("%s: sealed override must bind no token", addr)
		}
		if c.BindAddr != addr {
			t.Fatalf("%s: BindAddr must be the override, got %q", addr, c.BindAddr)
		}
	}
}

// A loopback --addr override keeps dev-fallback (local convenience preserved).
func TestLoad_LoopbackAddrOverrideDev(t *testing.T) {
	c, err := Load(env(map[string]string{}), "127.0.0.1:9999")
	if err != nil {
		t.Fatal(err)
	}
	if c.Public || c.TokenMode != TokenDevFallback {
		t.Fatalf("loopback --addr must stay dev-fallback, got public=%v mode=%v", c.Public, c.TokenMode)
	}
}

// A malformed --addr is rejected.
func TestLoad_BadAddrOverrideRejected(t *testing.T) {
	if _, err := Load(env(map[string]string{}), "not-an-addr"); err == nil {
		t.Fatal("malformed --addr must error")
	}
}

// REGRESSION (review finding): a provisioned token on a PUBLIC bind without the
// explicit acknowledgement is fail-closed (Load errors) — no live credential on a
// cleartext public socket by accident.
func TestLoad_ProvisionedPublicPlaintextRefused(t *testing.T) {
	_, err := Load(env(map[string]string{"PORT": "8080", "ARKWEN_OPERATOR_TOKEN": "s3cret"}), "")
	if err == nil {
		t.Fatal("provisioned token on a public plaintext bind must be refused without ARKWEN_ALLOW_INSECURE_PUBLIC")
	}
	// loopback provisioned is fine (not public)
	if _, err := Load(env(map[string]string{"ARKWEN_OPERATOR_TOKEN": "s3cret"}), "127.0.0.1:7777"); err != nil {
		t.Fatalf("provisioned token on loopback must be allowed: %v", err)
	}
}
