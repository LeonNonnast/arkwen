package redaction

import (
	"strings"
	"testing"
)

// Invariants 5/3: every non-secret byte is preserved; each secret span is
// replaced by "<redacted:RULE>"; the raw secret appears 0 times in the output.
func TestRedact_Fidelity(t *testing.T) {
	e := New()
	e.Register("model-api-key", "sk-live-SECRET")
	in := "Calling API with key sk-live-SECRET ... 200 OK"
	res := e.Redact([]byte(in))
	out := string(res.Sanitized)
	if strings.Contains(out, "sk-live-SECRET") {
		t.Fatalf("secret leaked: %q", out)
	}
	if out != "Calling API with key <redacted:model-api-key> ... 200 OK" {
		t.Fatalf("fidelity broken: %q", out)
	}
	if res.Total != 1 || res.PerRule["model-api-key"] != 1 {
		t.Fatalf("bad accounting: %+v", res)
	}
}

// Longest-first: a secret that is a substring of another must not corrupt the
// longer redaction.
func TestRedact_LongestFirst(t *testing.T) {
	e := New()
	e.Register("short", "abc")
	e.Register("long", "abcdef")
	res := e.Redact([]byte("value=abcdef and abc"))
	out := string(res.Sanitized)
	if strings.Contains(out, "abcdef") {
		t.Fatalf("longer secret leaked: %q", out)
	}
	if !strings.Contains(out, "<redacted:long>") || !strings.Contains(out, "<redacted:short>") {
		t.Fatalf("expected both tokens: %q", out)
	}
}

func TestContainsAndUnregister(t *testing.T) {
	e := New()
	e.Register("k", "topsecret")
	if !e.Contains([]byte("x topsecret y")) {
		t.Fatal("Contains should detect the secret")
	}
	e.Unregister("k")
	if e.Contains([]byte("x topsecret y")) {
		t.Fatal("Unregister should remove the rule")
	}
	if e.Redact([]byte("topsecret")).Total != 0 {
		t.Fatal("no redaction after unregister")
	}
}

// Empty secrets and empty rule ids are ignored (would otherwise redact everything).
func TestRedact_IgnoresEmpty(t *testing.T) {
	e := New()
	e.Register("k", "")
	e.Register("", "x")
	if e.Redact([]byte("hello")).Total != 0 {
		t.Fatal("empty registrations must be ignored")
	}
}
