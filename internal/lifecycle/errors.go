package lifecycle

import (
	"errors"
	"fmt"
)

// ErrCorruptState is the sentinel a *CorruptStateError wraps: the persisted
// desired-state row exists but holds a value outside {running,paused,
// stopped} (impossible today thanks to the DDL's CHECK constraint, but
// reachable from a future schema version or a manual edit — design v6 §8).
var ErrCorruptState = errors.New("lifecycle: persisted desired state is invalid")

// CorruptStateError reports an unrecognized persisted desired-state value,
// carrying the raw string so the fail-closed reconciliation path (design v6
// §5.4) can record it verbatim in the rewritten row's reason
// ("fail-closed: was '<raw>'") and in LastError.
type CorruptStateError struct {
	Raw string
}

func (e *CorruptStateError) Error() string {
	return fmt.Sprintf("lifecycle: invalid persisted desired state %q", e.Raw)
}

// Unwrap lets callers use errors.Is(err, ErrCorruptState) without caring
// about the concrete Raw payload.
func (e *CorruptStateError) Unwrap() error { return ErrCorruptState }

// ErrDirtyTeardown is the default sentinel the dirty-teardown classifier
// (IsDirtyTeardownError) recognizes via errors.Is. A teardown error in the
// "join timeout" class (design v6 §5.3) means orphaned goroutines may still
// be alive, so it is handled specially (degraded, or a process-exit
// depending on desired) rather than as an ordinary transition failure.
//
// b3 replaces IsDirtyTeardownError with a classifier that ALSO recognizes
// the real miner's errLoopJoinTimeout (via errors.Is) without this package
// ever importing internal/miner; tests inject their own fake Runner errors
// wrapping ErrDirtyTeardown to exercise the same code path.
var ErrDirtyTeardown = errors.New("lifecycle: dirty teardown (join-timeout class)")

// IsDirtyTeardownError classifies a teardown/generation error as belonging
// to the "join timeout" class described in design v6 §5.3. It is a
// package-level seam (not a Config field) so it can be swapped process-wide
// by whichever integration layer wires the real sentinel(s) in, exactly
// like startupBackoffSchedule is a package var in internal/miner.
var IsDirtyTeardownError = func(err error) bool {
	return errors.Is(err, ErrDirtyTeardown)
}

// ErrAlreadyRunning is returned by Controller.Run when called a second
// time on the same Controller (MINOR 16, F4b Q3 consolidated corrective):
// Run is single-use — a second call, concurrent or sequential, must never
// spawn a second worker goroutine racing the first over every
// worker-owned field with no synchronization at all.
var ErrAlreadyRunning = errors.New("lifecycle: Run already called")
