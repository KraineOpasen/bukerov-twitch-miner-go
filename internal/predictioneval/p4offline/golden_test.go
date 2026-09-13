package p4offline_test

// EXACT WIRE GOLDENS for the digested artifacts.
//
// A self-consistent round trip cannot establish a wire format. The expected
// framed bytes and digests below were produced by an INDEPENDENT Python
// implementation of the documented framing (testdata/synthetic/
// gen_golden_digests.py, see PROVENANCE.md "Canonical framing") over the same
// fixture values the tests build, so a change to the framing, the field
// order or the fixture is caught here by name.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

type goldenDigest struct {
	FramedHex string `json:"framedHex"`
	SHA256    string `json:"sha256"`
}

func loadGoldens(t *testing.T) map[string]goldenDigest {
	t.Helper()
	raw, err := os.ReadFile("testdata/synthetic/golden_digests.json")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]goldenDigest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"commonFactset", "p2ConfigBinding", "resolutionArtifact", "sourceRoundRegistry"} {
		if out[k].SHA256 == "" || out[k].FramedHex == "" {
			t.Fatalf("golden %q missing", k)
		}
	}
	return out
}

func TestCommonFactsetWireFormatMatchesTheIndependentGolden(t *testing.T) {
	g := loadGoldens(t)["commonFactset"]
	_, fs := selectedFactset(t, nil, nil)
	if got := hex.EncodeToString(p4offline.SerializeCommonFactset(fs)); got != g.FramedHex {
		t.Fatalf("framed bytes differ from the independent oracle:\n got  %s\n want %s", got, g.FramedHex)
	}
	if fs.Digest != g.SHA256 {
		t.Fatalf("digest %s, oracle %s", fs.Digest, g.SHA256)
	}
}

func TestP2ConfigBindingDigestMatchesTheIndependentGolden(t *testing.T) {
	g := loadGoldens(t)["p2ConfigBinding"]
	_, fs := selectedFactset(t, nil, nil)
	b, err := p4offline.BindP2Config(fs)
	if err != nil {
		t.Fatal(err)
	}
	if b.Model.PlatformIntBits != 64 {
		t.Skipf("the golden was derived for a 64-bit int platform; this platform has %d", b.Model.PlatformIntBits)
	}
	if b.Digest != g.SHA256 {
		t.Fatalf("digest %s, oracle %s", b.Digest, g.SHA256)
	}
}

func TestResolutionArtifactDigestMatchesTheIndependentGolden(t *testing.T) {
	g := loadGoldens(t)["resolutionArtifact"]
	a := p4offline.ProjectResolution(goodWinnerEvidence())
	if a.ResolutionFactsDigest != g.SHA256 {
		t.Fatalf("digest %s, oracle %s", a.ResolutionFactsDigest, g.SHA256)
	}
}

func TestSourceRoundRegistryDigestMatchesTheIndependentGolden(t *testing.T) {
	g := loadGoldens(t)["sourceRoundRegistry"]
	epA := p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"}
	keyA := predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", AttemptID: 1}
	reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{
		{Episode: epA, Attempt: keyA, FactsetDigest: "d1"},
		{Episode: epA, Attempt: keyA, FactsetDigest: "d1"},
		{Episode: p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", RoundIncarnationID: "r2", EventID: "e2"}, Attempt: keyA, FactsetDigest: "d2"},
	})
	if reg.Digest != g.SHA256 {
		t.Fatalf("digest %s, oracle %s", reg.Digest, g.SHA256)
	}
}
