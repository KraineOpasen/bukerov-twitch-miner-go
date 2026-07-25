package supportbundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fixedClock returns an Options.Now-compatible func pinned to t, so tests
// never sleep and never depend on wall-clock time.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// minimalInput is the smallest Input that exercises every always-present
// section (Health/Drops left nil - "nil source" / "subsystem not present" is
// covered by its own test).
func minimalInput() Input {
	return Input{
		AppVersion:    "1.2.3",
		GoVersion:     "go1.25.0",
		OS:            "linux",
		Arch:          "amd64",
		UptimeSeconds: 3600,
		MinerStatus:   "running",
		Runtime: RuntimeInfo{
			DashboardAuthMode: "authenticated",
			FeatureFlags:      FeatureFlags{DiscoveryEnabled: true, NotificationsEnabled: true},
			Intervals:         Intervals{CampaignSyncMinutes: 15, WatchTimeWindowHours: 24},
			Counts:            Counts{ConfiguredStreamers: 2, DiscoveredChannels: 1},
			Notifications:     NotificationsInfo{Enabled: true, Providers: []string{"discord"}, ConfigValid: true},
		},
		Watching: WatchingSection{
			Mode:                 "direct",
			WatchTimeWindowHours: 24,
			Slots: []WatchSlot{
				{Slot: 0, Channel: "streamerone", Source: "configured", ReasonCode: "priority"},
			},
			Streamers: []StreamerEntry{
				{Channel: "streamerone", Status: "online", Watching: true, Game: "Just Chatting"},
			},
			PubSub: PubSubSection{TotalTopics: 3, Connections: []PubSubConn{{Index: 0, Topics: 3}}},
		},
	}
}

func fullInput() Input {
	in := minimalInput()
	in.Health = &HealthSection{
		ActiveClient: "TV",
		Signals:      []HealthSignal{{Name: "oauth", Status: "ok"}},
	}
	in.Drops = &DropsSection{
		SyncStatus: DropsSyncStatus{SyncRuns: 4, TrackedCampaigns: 1},
		Campaigns:  []DropCampaign{{Name: "Summer Campaign", Game: "Just Chatting", RemainingDrops: 2}},
		ProgressWatchdog: &ProgressWatchdogSection{
			Enabled: true,
			Drops:   []DropProgress{{Campaign: "Summer Campaign", Drop: "Drop 1", Status: "healthy"}},
			Avoided: []AvoidedChannel{{Login: "avoidedchannel", Reason: "drop progress stalled on this channel despite session recovery"}},
		},
		Policy: &PolicySection{
			Mode:      "GAME_ORDER",
			Decisions: []PolicyDecision{{Campaign: "Summer Campaign", Status: "active", Total: 10}},
		},
	}
	in.Journals = JournalsSection{
		Slots:  []SlotEventRecord{{Seq: 1, Type: "entered", Channel: "streamerone"}},
		Health: []HealthEventRecord{{Seq: 1, Type: "transition", Domain: "connection", NewLevel: "healthy"}},
	}
	return in
}

// zipEntries reads back a Result's ZIP and returns name -> raw bytes.
func zipEntries(t *testing.T, result Result) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(result.Bytes), int64(len(result.Bytes)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	out := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read entry %s: %v", f.Name, err)
		}
		_ = rc.Close()
		out[f.Name] = buf.Bytes()
	}
	return out
}

// T1: valid ZIP, expected constant entries, every JSON entry parses, and
// manifest.schemaVersion == 1.
func TestBuildProducesExpectedEntries(t *testing.T) {
	clock := fixedClock(time.Date(2026, 7, 24, 10, 30, 0, 0, time.UTC))
	result, err := Build(fullInput(), Options{Now: clock})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	entries := zipEntries(t, result)
	want := []string{
		"manifest.json", "runtime.json", "health.json", "watching.json",
		"drops.json", "journals/slots.json", "journals/health.json", "README.txt",
	}
	for _, name := range want {
		data, ok := entries[name]
		if !ok {
			t.Fatalf("missing entry %q; got %v", name, entryNamesOf(entries))
		}
		if name != "README.txt" {
			var v any
			if err := json.Unmarshal(data, &v); err != nil {
				t.Errorf("entry %q is not valid JSON: %v", name, err)
			}
		}
	}
	if len(entries) != len(want) {
		t.Errorf("got %d entries, want exactly %d: %v", len(entries), len(want), entryNamesOf(entries))
	}

	var manifest manifestDoc
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Errorf("manifest.schemaVersion = %d, want 1", manifest.SchemaVersion)
	}
	if manifest.GeneratedAt.IsZero() {
		t.Error("manifest.generatedAt is zero")
	}
	for _, name := range want {
		found := false
		for _, f := range manifest.IncludedFiles {
			if f == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("manifest.includedFiles missing %q: %v", name, manifest.IncludedFiles)
		}
	}
}

func entryNamesOf(m map[string][]byte) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

// Handling a nil/empty source: Health and Drops omitted, everything else
// still present and structurally valid.
func TestBuildHandlesEmptyInputGracefully(t *testing.T) {
	result, err := Build(Input{}, Options{Now: fixedClock(time.Unix(0, 0).UTC())})
	if err != nil {
		t.Fatalf("Build with zero-value Input: %v", err)
	}
	entries := zipEntries(t, result)
	for _, absent := range []string{"health.json", "drops.json"} {
		if _, ok := entries[absent]; ok {
			t.Errorf("zero-value Input should omit %q", absent)
		}
	}
	for _, present := range []string{"manifest.json", "runtime.json", "watching.json", "journals/slots.json", "journals/health.json", "README.txt"} {
		if _, ok := entries[present]; !ok {
			t.Errorf("zero-value Input should still include %q", present)
		}
	}
}

// T17: ZIP safety - constant, safe entry names; no traversal, no absolute
// path, no duplicates, no symlink/exec bits; explicit Deflate method.
func TestZipEntrySafety(t *testing.T) {
	result, err := Build(fullInput(), Options{Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(result.Bytes), int64(len(result.Bytes)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}

	seen := map[string]bool{}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") {
			t.Errorf("entry name contains \"..\": %q", f.Name)
		}
		if strings.HasPrefix(f.Name, "/") {
			t.Errorf("entry name is absolute: %q", f.Name)
		}
		if seen[f.Name] {
			t.Errorf("duplicate entry name: %q", f.Name)
		}
		seen[f.Name] = true

		mode := f.Mode()
		if mode&0o111 != 0 {
			t.Errorf("entry %q has an executable bit set: %v", f.Name, mode)
		}
		if mode&0o777 != 0o644 {
			t.Errorf("entry %q mode = %v, want 0644", f.Name, mode)
		}
		// A symlink is a regular file whose mode has the symlink type bit
		// set; explicit SetMode(0o644) never sets it, but assert it anyway
		// as a hard safety net against a future refactor.
		if mode&os.ModeSymlink != 0 {
			t.Errorf("entry %q looks like a symlink: %v", f.Name, mode)
		}
		if f.Method != zip.Deflate {
			t.Errorf("entry %q method = %v, want Deflate", f.Name, f.Method)
		}
	}
}

// T18: bounded journals - newest kept, truncation metadata correct, and the
// output never allocates more than the documented cap worth of records.
func TestJournalTruncationKeepsNewest(t *testing.T) {
	const total = maxJournalRecords + 37
	var records []SlotEventRecord
	for i := 1; i <= total; i++ {
		records = append(records, SlotEventRecord{Seq: uint64(i), Type: "entered", Channel: fmt.Sprintf("chan%d", i)})
	}
	in := minimalInput()
	in.Journals = JournalsSection{Slots: records}

	result, err := Build(in, Options{Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	entries := zipEntries(t, result)

	var doc journalDoc[slotEventDoc]
	if err := json.Unmarshal(entries["journals/slots.json"], &doc); err != nil {
		t.Fatalf("unmarshal journals/slots.json: %v", err)
	}
	if !doc.Truncated {
		t.Error("expected Truncated=true")
	}
	if doc.Included != maxJournalRecords {
		t.Errorf("Included = %d, want %d", doc.Included, maxJournalRecords)
	}
	if doc.Omitted != total-maxJournalRecords {
		t.Errorf("Omitted = %d, want %d", doc.Omitted, total-maxJournalRecords)
	}
	if doc.LastSeq != uint64(total) {
		t.Errorf("LastSeq = %d, want %d (the newest observed, regardless of truncation)", doc.LastSeq, total)
	}
	if len(doc.Records) != maxJournalRecords {
		t.Fatalf("len(Records) = %d, want %d", len(doc.Records), maxJournalRecords)
	}
	// Newest kept: the retained records must be the LAST `maxJournalRecords`
	// sequence numbers (total-maxJournalRecords+1 .. total), not the first.
	firstKept := doc.Records[0].Seq
	lastKept := doc.Records[len(doc.Records)-1].Seq
	if firstKept != uint64(total-maxJournalRecords+1) {
		t.Errorf("first kept Seq = %d, want %d (oldest of the retained newest set)", firstKept, total-maxJournalRecords+1)
	}
	if lastKept != uint64(total) {
		t.Errorf("last kept Seq = %d, want %d", lastKept, total)
	}

	// Manifest also records the same truncation under a qualified key.
	var manifest manifestDoc
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	tr, ok := manifest.Truncations["journals.slots"]
	if !ok {
		t.Fatal("manifest.truncations missing \"journals.slots\"")
	}
	if !tr.Truncated || tr.Included != maxJournalRecords || tr.Omitted != total-maxJournalRecords {
		t.Errorf("manifest truncation entry = %+v, want Included=%d Omitted=%d Truncated=true", tr, maxJournalRecords, total-maxJournalRecords)
	}
}

// Untruncated journals report Truncated=false with the exact counts, and
// LastSeq is still the newest sequence number.
func TestJournalNoTruncationWhenUnderCap(t *testing.T) {
	in := minimalInput()
	in.Journals = JournalsSection{
		Health: []HealthEventRecord{{Seq: 1, Type: "transition"}, {Seq: 2, Type: "transition"}},
	}
	result, err := Build(in, Options{Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	entries := zipEntries(t, result)
	var doc journalDoc[healthEventDoc]
	if err := json.Unmarshal(entries["journals/health.json"], &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Truncated {
		t.Error("expected Truncated=false under the cap")
	}
	if doc.Included != 2 || doc.Omitted != 0 {
		t.Errorf("Included=%d Omitted=%d, want 2/0", doc.Included, doc.Omitted)
	}
	if doc.LastSeq != 2 {
		t.Errorf("LastSeq = %d, want 2", doc.LastSeq)
	}
	if len(doc.Records) != 2 {
		t.Fatalf("len(Records) = %d, want 2", len(doc.Records))
	}
}

// T19: bounded strings - a long benign string is truncated to maxStringLen,
// and a long string containing a sensitive pattern is fully redacted (never
// partially - the marker never reveals a prefix of the secret).
func TestStringBoundsAppliedEndToEnd(t *testing.T) {
	longSafe := strings.Repeat("a", maxStringLen+250)
	longUnsafe := "Bearer " + strings.Repeat("x", 100)

	in := minimalInput()
	in.Watching.Streamers = []StreamerEntry{
		{Channel: "c1", Status: "online", Game: longSafe},
		{Channel: "c2", Status: "online", Game: longUnsafe},
	}
	result, err := Build(in, Options{Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	entries := zipEntries(t, result)
	var doc watchingDoc
	if err := json.Unmarshal(entries["watching.json"], &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Streamers) != 2 {
		t.Fatalf("len(Streamers) = %d, want 2", len(doc.Streamers))
	}
	if got := len([]rune(doc.Streamers[0].Game)); got != maxStringLen {
		t.Errorf("long safe string len = %d, want exactly %d", got, maxStringLen)
	}
	if doc.Streamers[1].Game != redactedMarker {
		t.Errorf("long unsafe string = %q, want the redaction marker", doc.Streamers[1].Game)
	}
	if strings.Contains(doc.Streamers[1].Game, "x") {
		t.Error("redacted value leaks a fragment of the original secret")
	}
}

// T20: size cap - an oversized fixture (far more items than any per-section
// bound) must be safely truncated to the documented caps; the resulting ZIP
// must never exceed maxZipBytes, and Build must not error given a realistic
// (if extreme) input shape.
func TestSizeCapWithOversizedFixture(t *testing.T) {
	in := minimalInput()

	streamers := make([]StreamerEntry, 5000)
	for i := range streamers {
		streamers[i] = StreamerEntry{Channel: fmt.Sprintf("streamer%d", i), Status: "offline"}
	}
	in.Watching.Streamers = streamers

	campaigns := make([]DropCampaign, 5000)
	for i := range campaigns {
		campaigns[i] = DropCampaign{Name: fmt.Sprintf("Campaign %d", i), RemainingDrops: i}
	}
	in.Drops = &DropsSection{Campaigns: campaigns}

	slots := make([]SlotEventRecord, 50000)
	for i := range slots {
		slots[i] = SlotEventRecord{Seq: uint64(i + 1), Type: "entered", Channel: fmt.Sprintf("chan%d", i)}
	}
	in.Journals = JournalsSection{Slots: slots}

	result, err := Build(in, Options{Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("Build with oversized fixture returned an error instead of a safe truncation: %v", err)
	}
	if len(result.Bytes) > maxZipBytes {
		t.Errorf("zip size %d exceeds the documented bound %d", len(result.Bytes), maxZipBytes)
	}

	entries := zipEntries(t, result)
	var watching watchingDoc
	if err := json.Unmarshal(entries["watching.json"], &watching); err != nil {
		t.Fatalf("unmarshal watching.json: %v", err)
	}
	if len(watching.Streamers) != maxStreamers {
		t.Errorf("len(Streamers) = %d, want the bounded cap %d", len(watching.Streamers), maxStreamers)
	}

	var drops dropsDoc
	if err := json.Unmarshal(entries["drops.json"], &drops); err != nil {
		t.Fatalf("unmarshal drops.json: %v", err)
	}
	if len(drops.Campaigns) != maxDropCampaigns {
		t.Errorf("len(Campaigns) = %d, want the bounded cap %d", len(drops.Campaigns), maxDropCampaigns)
	}

	var journal journalDoc[slotEventDoc]
	if err := json.Unmarshal(entries["journals/slots.json"], &journal); err != nil {
		t.Fatalf("unmarshal journals/slots.json: %v", err)
	}
	if len(journal.Records) != maxJournalRecords {
		t.Errorf("len(Records) = %d, want the bounded cap %d", len(journal.Records), maxJournalRecords)
	}
}

// T21: stable ordering - the same Input and the same fixed clock produce
// byte-identical ZIP output, run after run.
func TestBuildIsByteStable(t *testing.T) {
	in := fullInput()
	clock := fixedClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	first, err := Build(in, Options{Now: clock})
	if err != nil {
		t.Fatalf("Build (1st): %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Build(in, Options{Now: clock})
		if err != nil {
			t.Fatalf("Build (run %d): %v", i, err)
		}
		if !bytes.Equal(first.Bytes, again.Bytes) {
			t.Fatalf("run %d produced different bytes than the first run (len %d vs %d)", i, len(again.Bytes), len(first.Bytes))
		}
		if again.Filename != first.Filename {
			t.Fatalf("run %d filename %q != first %q", i, again.Filename, first.Filename)
		}
	}
}

// T22: the fake clock alone controls generatedAt and the filename - no
// sleeps, no dependency on real time.
func TestOptionsClockControlsFilenameAndGeneratedAt(t *testing.T) {
	when := time.Date(2030, 12, 31, 23, 59, 1, 0, time.UTC)
	result, err := Build(minimalInput(), Options{Now: fixedClock(when)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantFilename := "bukerov-support-20301231T235901Z.zip"
	if result.Filename != wantFilename {
		t.Errorf("Filename = %q, want %q", result.Filename, wantFilename)
	}

	entries := zipEntries(t, result)
	var manifest manifestDoc
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !manifest.GeneratedAt.Equal(when) {
		t.Errorf("manifest.GeneratedAt = %v, want %v", manifest.GeneratedAt, when)
	}
}

// A nil Options.Now falls back to time.Now (still no error, still a valid
// filename shape) - this only proves the fallback doesn't panic/misbehave,
// it deliberately does NOT assert on the exact value (that would be a sleep
// -free test asserting on real wall-clock time, which is exactly what T22
// tells us to avoid relying on).
func TestNilClockFallsBackToTimeNow(t *testing.T) {
	result, err := Build(minimalInput(), Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.HasPrefix(result.Filename, "bukerov-support-") || !strings.HasSuffix(result.Filename, "Z.zip") {
		t.Errorf("Filename = %q, want the bukerov-support-...Z.zip shape", result.Filename)
	}
}
