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

// nopSink is Config's default StatusSink: every call is a no-op, so a
// Controller built without one behaves identically to having no status side
// channel at all.
type nopSink struct{}

func (nopSink) SetStatus(string, string) {}
func (nopSink) SetGeneration(uint64)     {}
