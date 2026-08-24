package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

func validStableStatementClaims(t *testing.T, rel *release, repo, tag, commit string) stableStatementClaims {
	t.Helper()
	subjects := make([]map[string]any, 0, len(stableBinaryAssetNames))
	for _, name := range stableBinaryAssetNames {
		a, err := findUniqueAsset(rel, name)
		if err != nil || a == nil {
			t.Fatalf("find %s: asset=%v err=%v", name, a, err)
		}
		subjects = append(subjects, map[string]any{
			"name": name,
			"digest": map[string]string{
				"sha256": strings.TrimPrefix(a.Digest, "sha256:"),
			},
		})
	}
	repoURI := "https://github.com/" + repo
	workflowURI := stableWorkflowURI(repo, tag)
	raw := map[string]any{
		"_type":         stableInTotoStatementType,
		"predicateType": stableSLSAPredicateType,
		"subject":       subjects,
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": "https://actions.github.io/buildtypes/workflow/v1",
				"externalParameters": map[string]any{
					"workflow": map[string]string{
						"ref":        "refs/tags/" + tag,
						"repository": repoURI,
						"path":       stableWorkflowPath,
					},
				},
				"resolvedDependencies": []map[string]any{{
					"uri": "git+" + repoURI + "@refs/tags/" + tag,
					"digest": map[string]string{
						"gitCommit": commit,
					},
				}},
			},
			"runDetails": map[string]any{
				"builder": map[string]string{"id": workflowURI},
			},
		},
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var claims stableStatementClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestVerifyStableStatementClaimsBindsExactProducerAndBothBinaries(t *testing.T) {
	const (
		repo   = "KraineOpasen/bukerov-twitch-miner-go"
		tag    = "stable-v0.1.3"
		commit = "22e7eac1b5549b42cf6387ddebfb818fc82a3b97"
	)
	rel := completeStableRelease(tag)
	current, err := findUniqueAsset(&rel, "twitch-miner-go-linux-amd64")
	if err != nil || current == nil {
		t.Fatalf("current asset=%v err=%v", current, err)
	}
	digest := strings.TrimPrefix(current.Digest, "sha256:")
	repoURI := "https://github.com/" + repo
	workflowURI := stableWorkflowURI(repo, tag)

	valid := validStableStatementClaims(t, &rel, repo, tag, commit)
	if err := verifyStableStatementClaims(
		valid, &rel, current.Name, digest, repoURI, workflowURI, tag, commit,
	); err != nil {
		t.Fatalf("valid signed claims rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*stableStatementClaims)
	}{
		{"main ref", func(c *stableStatementClaims) {
			c.Predicate.BuildDefinition.ExternalParameters.Workflow.Ref = "refs/heads/main"
		}},
		{"lookalike workflow", func(c *stableStatementClaims) {
			c.Predicate.BuildDefinition.ExternalParameters.Workflow.Path = ".github/workflows/main-release.yml"
		}},
		{"wrong builder", func(c *stableStatementClaims) {
			c.Predicate.RunDetails.Builder.ID = strings.Replace(workflowURI, "stable-release", "release", 1)
		}},
		{"wrong source commit", func(c *stableStatementClaims) {
			c.Predicate.BuildDefinition.ResolvedDependencies[0].Digest["gitCommit"] = strings.Repeat("f", 40)
		}},
		{"missing other architecture", func(c *stableStatementClaims) {
			c.Subject = c.Subject[:1]
		}},
		{"tampered other architecture", func(c *stableStatementClaims) {
			c.Subject[1].Digest["sha256"] = strings.Repeat("0", 64)
		}},
		{"extra digest algorithm", func(c *stableStatementClaims) {
			c.Subject[0].Digest["sha512"] = "00"
		}},
		{"wrong predicate", func(c *stableStatementClaims) {
			c.PredicateType = "https://example.test/not-slsa"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := validStableStatementClaims(t, &rel, repo, tag, commit)
			tc.mutate(&claims)
			if err := verifyStableStatementClaims(
				claims, &rel, current.Name, digest, repoURI, workflowURI, tag, commit,
			); err == nil {
				t.Fatal("mutated provenance claims qualified")
			}
		})
	}
}

func TestStableCertificateIdentityPinsTagCommitAndWorkflow(t *testing.T) {
	const (
		owner  = "KraineOpasen"
		repo   = "KraineOpasen/bukerov-twitch-miner-go"
		tag    = "stable-v0.1.3"
		commit = "22e7eac1b5549b42cf6387ddebfb818fc82a3b97"
	)
	repoURI := "https://github.com/" + repo
	workflowURI := stableWorkflowURI(repo, tag)
	identity, err := stableCertificateIdentity(owner, repoURI, workflowURI, tag, commit)
	if err != nil {
		t.Fatal(err)
	}
	summary := certificate.Summary{
		SubjectAlternativeName: workflowURI,
		Extensions: certificate.Extensions{
			Issuer:                              stableGitHubOIDCIssuer,
			BuildSignerURI:                      workflowURI,
			BuildSignerDigest:                   commit,
			RunnerEnvironment:                   stableGitHubRunner,
			SourceRepositoryURI:                 repoURI,
			SourceRepositoryDigest:              commit,
			SourceRepositoryRef:                 "refs/tags/" + tag,
			SourceRepositoryOwnerURI:            "https://github.com/" + owner,
			BuildConfigURI:                      workflowURI,
			BuildConfigDigest:                   commit,
			BuildTrigger:                        "push",
			SourceRepositoryVisibilityAtSigning: "public",
		},
	}
	if err := identity.Verify(summary); err != nil {
		t.Fatalf("exact certificate identity rejected: %v", err)
	}
	summary.SourceRepositoryRef = "refs/heads/main"
	if err := identity.Verify(summary); err == nil {
		t.Fatal("main certificate ref qualified for stable")
	}
}

func TestResolveStableTagCommitFollowsOnlyBoundedGitTagObjects(t *testing.T) {
	tagObject := strings.Repeat("a", 40)
	commit := strings.Repeat("b", 40)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var objectType, sha string
		switch r.URL.Path {
		case "/repos/owner/repo/git/ref/tags/stable-v0.1.3":
			objectType, sha = "tag", tagObject
		case "/repos/owner/repo/git/tags/" + tagObject:
			objectType, sha = "commit", commit
		default:
			t.Fatalf("unexpected tag-resolution path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]string{"type": objectType, "sha": sha},
		})
	}))
	defer srv.Close()

	got, err := resolveStableTagCommit(
		context.Background(), srv.Client(), srv.URL, "owner/repo", "stable-v0.1.3",
	)
	if err != nil || got != commit || requests.Load() != 2 {
		t.Fatalf("commit=%q requests=%d err=%v; want %q, 2, nil", got, requests.Load(), err, commit)
	}
}

func TestStableAttestationURLUsesExactDigestPredicateAndPagination(t *testing.T) {
	digest := strings.Repeat("a", 64)
	raw := stableAttestationsURL("https://api.github.test/", "owner/repo", digest, 7)
	request, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := fmt.Sprintf("/repos/owner/repo/attestations/sha256:%s", digest)
	if request.URL.Path != wantPath ||
		request.URL.Query().Get("predicate_type") != stableSLSAPredicateType ||
		request.URL.Query().Get("per_page") != "100" ||
		request.URL.Query().Get("page") != "7" {
		t.Fatalf("attestation URL = %s", raw)
	}
}
