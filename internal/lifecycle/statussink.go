package lifecycle

// StatusSink is the outbound port b3 wires to the existing web status
// broadcaster (design v6 §14: "Исходящий порт статуса — граница Ф4b/Ф4c").
// b1 ships only the no-op default (nopSink); this package must never import
// internal/web or assume any concrete status string beyond passing whatever
// it is given through verbatim — mapping lifecycle concepts onto the
// existing web.MinerStatus whitelist is entirely b3's/Ф4c's job.
type StatusSink interface {
	// SetStatus mirrors web.Server's existing SetStatus(status, message)
	// signature so a b3 adapter is a direct pass-through.
	SetStatus(status, message string)
	// SetGeneration reports the generation token BEFORE that generation's
	// Run is launched (design v6 §10: "SetGeneration(n) ДО запуска Run"),
	// so a client-visible discriminator never lags the status it labels.
	SetGeneration(uint64)
}

// BootHonoredIntentMessage is the message SetStatus carries EXACTLY once
// per boot, only for a boot-honored persisted paused/stopped intent (design
// v6 §5.4, contract §11 item 9) — MINOR 13, F4b Q3 consolidated corrective.
// It is a marker, not free text: an integration adapter (e.g. b3's web
// status adapter) matches on it EXACTLY to distinguish "the miner never
// started at all, and nothing else will ever explain why" from an ordinary
// runtime paused/stopped SetStatus call (an operator explicitly pausing or
// stopping right now, which publishTerminal also routes through the same
// SetStatus method) — only the former needs a special, standing overlay
// message; the latter is communicated through the ordinary lifecycle
// command surface instead.
const BootHonoredIntentMessage = "persisted lifecycle intent honored"

// nopSink is Config's default StatusSink: every call is a no-op, so a
// Controller built without one behaves identically to having no status side
// channel at all.
type nopSink struct{}

func (nopSink) SetStatus(string, string) {}
func (nopSink) SetGeneration(uint64)     {}
