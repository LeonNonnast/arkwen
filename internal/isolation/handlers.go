package isolation

import (
	"fmt"
	"os"
	"os/exec"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// local backs the STANDARD profile. On a production Linux host this is runc with
// the full hardening posture (no-new-privileges, dropped caps, seccomp,
// read-only rootfs, non-root) behind host-side default-deny egress. On WSL2 the
// worker runs in-process behind the WorkcellAPI boundary (which itself enforces
// the egress floor + redaction), so `standard` is always available for the
// walking skeleton. The security floor is identical either way (Invariant 9).
type localRuntime struct{}

func newLocal() Runtime { return &localRuntime{} }

func (l *localRuntime) Profile() arkwenv1.IsolationProfile {
	return arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD
}
func (l *localRuntime) Available() error { return nil }
func (l *localRuntime) Describe() string {
	return "standard: runc-class hardening (no-new-privileges, dropped caps, seccomp, read-only rootfs, non-root); host-side default-deny egress"
}

// gVisorRuntime backs the HARDENED profile via gVisor/runsc. Fail-closed: absent
// runsc means the profile is unsatisfiable, NEVER auto-downgraded to standard.
type gVisorRuntime struct{}

func newGVisor() Runtime { return &gVisorRuntime{} }

func (g *gVisorRuntime) Profile() arkwenv1.IsolationProfile {
	return arkwenv1.IsolationProfile_ISOLATION_PROFILE_HARDENED
}
func (g *gVisorRuntime) Available() error {
	if _, err := exec.LookPath("runsc"); err != nil {
		return fmt.Errorf("gVisor runsc not found: %w", err)
	}
	return nil
}
func (g *gVisorRuntime) Describe() string {
	return "hardened: gVisor/runsc user-space kernel; same default-deny egress floor as standard"
}

// firecrackerRuntime backs the STRICT profile via a Firecracker microVM + jailer,
// transport = virtio-vsock. Fail-closed until a capable host (bare-metal /
// nested-virt) exists — dev on WSL2 typically lacks nested virt. NO auto-downgrade.
type firecrackerRuntime struct{}

func newFirecracker() Runtime { return &firecrackerRuntime{} }

func (f *firecrackerRuntime) Profile() arkwenv1.IsolationProfile {
	return arkwenv1.IsolationProfile_ISOLATION_PROFILE_STRICT
}
func (f *firecrackerRuntime) Available() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("no /dev/kvm (nested virtualization required for strict tier): %w", err)
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		return fmt.Errorf("firecracker binary not found: %w", err)
	}
	return nil
}
func (f *firecrackerRuntime) Describe() string {
	return "strict: Firecracker microVM + jailer over virtio-vsock; wired but fail-closed until a nested-virt/bare-metal host"
}
