package isolation

import (
	"time"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// ResourceSample is a cgroups v2 usage snapshot (ADR-006 E3). wall_clock counts
// only running time — suspension is excluded (a run waiting on a human gate is
// never killed for wall-clock).
type ResourceSample struct {
	CPUMillicores    uint64
	MemBytes         uint64
	DiskBytes        uint64
	Pids             uint64
	WallClockRunning time.Duration
}

// CheckCeiling reports whether a usage sample breaches a HARD physical ceiling
// (org_cap-bound, NOT the topup-able cost budget — R5). A breach terminates the
// run with TERMINAL_REASON_RESOURCE_EXHAUSTED — a reason on FAILED, never a new
// state and never an auto-downgrade (Invariant 7/8). A zero limit = no ceiling on
// that dimension. On a production host the enforcement is cgroups v2 itself; this
// checker is the same decision made explicit for sampling + the budget gate.
func CheckCeiling(limits *arkwenv1.ResourceLimits, s ResourceSample) (breached bool, dimension string) {
	if limits == nil {
		return false, ""
	}
	if l := limits.GetMemBytes(); l != 0 && s.MemBytes > l {
		return true, "mem_bytes"
	}
	if l := limits.GetDiskBytes(); l != 0 && s.DiskBytes > l {
		return true, "disk_bytes"
	}
	if l := limits.GetPids(); l != 0 && s.Pids > l {
		return true, "pids"
	}
	if l := limits.GetCpuMillicores(); l != 0 && s.CPUMillicores > l {
		return true, "cpu_millicores"
	}
	if l := limits.GetWallClock().AsDuration(); l != 0 && s.WallClockRunning > l {
		return true, "wall_clock"
	}
	return false, ""
}

// ResourceExhaustedTermination is the fail-closed terminal for a hard-ceiling
// breach (ADR-006 E3).
func ResourceExhaustedTermination(dimension string) *arkwenv1.Termination {
	return &arkwenv1.Termination{
		State:  arkwenv1.TerminalState_TERMINAL_STATE_FAILED,
		Reason: arkwenv1.TerminalReason_TERMINAL_REASON_RESOURCE_EXHAUSTED,
		Detail: "hard resource ceiling breached: " + dimension,
	}
}
