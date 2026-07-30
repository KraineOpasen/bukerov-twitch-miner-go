package lifecycle

import (
	"fmt"
	"sync/atomic"
)

// commandSeq is a process-wide monotonic counter backing newCommandID.
// Command IDs only need to be unique within one process's lifetime (they
// identify an in-flight command to its own caller and to observability, not
// a cross-process/durable identity — the durable row separately stores its
// own command_id column verbatim), so a counter is simpler and allocation-
// free compared to a random ID generator.
var commandSeq atomic.Uint64

// newCommandID returns a process-unique, human-scannable command
// identifier (e.g. in logs and the GET /api/lifecycle response).
func newCommandID() string {
	return fmt.Sprintf("lc-%d", commandSeq.Add(1))
}
