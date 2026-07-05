// Package conformance is the adversarial golden-vector suite that pins the ten
// Arkwen invariants to the frozen wire contract AND to the reference runtime.
// Each vector file under conformance/ is mirrored here by Go assertions
// (G* golden-run, AT* adapter-tiers, R* redaction, D* reproduce, F* fail-closed).
//
// Structural assertions ("no bytes content field", "no credential field", "no
// consumer *_url field", "DEGRADED is not a state") are checked against the
// compiled protobuf descriptor set — that is where Invariants 4 and 5 actually
// live. Behavioural assertions drive the real runtime (internal/app).
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const vectorsDir = "../../conformance"

// loadVectorRaw decodes a vector file into a generic map.
func loadVectorRaw(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorsDir, name))
	if err != nil {
		t.Fatalf("read vector %s: %v", name, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode vector %s: %v", name, err)
	}
	return m
}

// unmarshalEnvelope parses a canonical proto3 JSON object into an EventEnvelope.
func unmarshalEnvelope(t *testing.T, raw json.RawMessage) *arkwenv1.EventEnvelope {
	t.Helper()
	ev := &arkwenv1.EventEnvelope{}
	if err := protojson.Unmarshal(raw, ev); err != nil {
		t.Fatalf("protojson unmarshal envelope: %v\n%s", err, string(raw))
	}
	return ev
}

// ---- descriptor-level structural helpers (Invariants 4/5/10) ----

// rangeArkwenMessages walks every message descriptor in package arkwen.v1.
func rangeArkwenMessages(fn func(md protoreflect.MessageDescriptor)) {
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() != "arkwen.v1" {
			return true
		}
		var walk func(mds protoreflect.MessageDescriptors)
		walk = func(mds protoreflect.MessageDescriptors) {
			for i := 0; i < mds.Len(); i++ {
				md := mds.Get(i)
				fn(md)
				walk(md.Messages())
			}
		}
		walk(fd.Messages())
		return true
	})
}

// bytesContentFields returns any bytes fields in the package (there must be none:
// content lives in the CAS, the wire carries pointers only — Invariants 4/5).
func bytesContentFields() []string {
	var out []string
	rangeArkwenMessages(func(md protoreflect.MessageDescriptor) {
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if f.Kind() == protoreflect.BytesKind {
				out = append(out, string(md.FullName())+"."+string(f.Name()))
			}
		}
	})
	return out
}

// fieldsMatching returns fully-qualified field names whose name contains any of
// the given (lowercased) needles, optionally restricted to a message.
func fieldsMatching(messageName string, needles ...string) []string {
	var out []string
	rangeArkwenMessages(func(md protoreflect.MessageDescriptor) {
		if messageName != "" && string(md.Name()) != messageName {
			return
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			name := strings.ToLower(string(f.Name()))
			for _, n := range needles {
				if strings.Contains(name, n) {
					out = append(out, string(md.FullName())+"."+string(f.Name()))
				}
			}
		}
	})
	return out
}

// enumValueName returns the value name for an enum number, or "".
func enumValueName(enumFullName protoreflect.FullName, number int32) string {
	ed, err := protoregistry.GlobalTypes.FindEnumByName(enumFullName)
	if err != nil {
		return ""
	}
	v := ed.Descriptor().Values().ByNumber(protoreflect.EnumNumber(number))
	if v == nil {
		return ""
	}
	return string(v.Name())
}
