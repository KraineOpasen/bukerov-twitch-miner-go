package version

import "testing"

func TestStableArtifactIdentityRoundTrip(t *testing.T) {
	marker := StableArtifactIdentity("0.1.4", "linux", "amd64")
	want := "BTM_STABLE_ARTIFACT_V1|VERSION=0.1.4|CHANNEL=stable|GOOS=linux|GOARCH=amd64"
	if marker != want {
		t.Fatalf("marker = %q, want %q", marker, want)
	}
	got, err := ParseArtifactIdentity(marker)
	if err != nil {
		t.Fatalf("ParseArtifactIdentity: %v", err)
	}
	if got.Version != "0.1.4" || got.Channel != "stable" || got.GOOS != "linux" || got.GOARCH != "amd64" {
		t.Fatalf("parsed identity = %#v", got)
	}
}

func TestParseArtifactIdentityRejectsNonStableAndNonCanonicalValues(t *testing.T) {
	for _, value := range []string{
		"",
		"BTM_STABLE_ARTIFACT_V1|VERSION=0.1.4|CHANNEL=main|GOOS=linux|GOARCH=amd64",
		"BTM_STABLE_ARTIFACT_V1|VERSION=00.1.4|CHANNEL=stable|GOOS=linux|GOARCH=amd64",
		"BTM_STABLE_ARTIFACT_V1|VERSION=0.1.4-rc.1|CHANNEL=stable|GOOS=linux|GOARCH=amd64",
		"prefix-BTM_STABLE_ARTIFACT_V1|VERSION=0.1.4|CHANNEL=stable|GOOS=linux|GOARCH=amd64",
		"BTM_STABLE_ARTIFACT_V1|VERSION=0.1.4|CHANNEL=stable|GOOS=linux|GOARCH=amd64|extra=x",
	} {
		if _, err := ParseArtifactIdentity(value); err == nil {
			t.Errorf("ParseArtifactIdentity(%q) succeeded, want rejection", value)
		}
	}
}
