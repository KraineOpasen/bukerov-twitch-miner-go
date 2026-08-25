package updater

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	sigverify "github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	stableWorkflowPath          = ".github/workflows/stable-release.yml"
	stableSLSAPredicateType     = "https://slsa.dev/provenance/v1"
	stableInTotoStatementType   = "https://in-toto.io/Statement/v1"
	stableGitHubOIDCIssuer      = "https://token.actions.githubusercontent.com"
	stableGitHubRunner          = "github-hosted"
	stableAttestationPageSize   = 100
	maxStableAttestationPages   = 10
	maxStableAPIResponseSize    = 16 << 20
	maxStableGitTagIndirections = 8
	stableTrustedRootCacheDays  = 1
)

var (
	stableRepoComponentRE = regexp.MustCompile("^[A-Za-z0-9_.-]+$")
	gitCommitRE           = regexp.MustCompile("^[0-9a-f]{40}$")
)

type stableProvenanceEvidence struct {
	SourceCommit string
}

type stableProvenanceVerifier func(context.Context, *release, string, string) (stableProvenanceEvidence, error)

type stableAttestationRecord struct {
	Bundle    json.RawMessage `json:"bundle"`
	Initiator string          `json:"initiator"`
}

type stableAttestationsResponse struct {
	Attestations []stableAttestationRecord `json:"attestations"`
}

type stableGitObject struct {
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type stableStatementClaims struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		BuildDefinition struct {
			BuildType          string `json:"buildType"`
			ExternalParameters struct {
				Workflow struct {
					Ref        string `json:"ref"`
					Repository string `json:"repository"`
					Path       string `json:"path"`
				} `json:"workflow"`
			} `json:"externalParameters"`
			ResolvedDependencies []struct {
				URI    string            `json:"uri"`
				Digest map[string]string `json:"digest"`
			} `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder struct {
				ID string `json:"id"`
			} `json:"builder"`
		} `json:"runDetails"`
	} `json:"predicate"`
}

// verifyStableProvenance accepts a stable candidate only when GitHub returns
// a cryptographically valid public-good Sigstore bundle for the exact binary
// digest. The certificate and signed SLSA statement are both pinned to this
// repository, the exact stable workflow, the exact tag ref, and the commit to
// which that tag resolves. A checksum or mutable Release record alone is not
// treated as provenance.
func verifyStableProvenance(
	ctx context.Context,
	client *http.Client,
	apiBaseURL, repo, cacheDir string,
	rel *release,
	assetName, digest string,
) (stableProvenanceEvidence, error) {
	owner, err := validateStableRepo(repo)
	if err != nil {
		return stableProvenanceEvidence{}, err
	}
	if _, _, ok := parseStableTag(rel.TagName); !ok {
		return stableProvenanceEvidence{}, fmt.Errorf("invalid stable provenance tag %q", rel.TagName)
	}
	if !sha256DigestRE.MatchString(digest) {
		return stableProvenanceEvidence{}, fmt.Errorf("invalid stable provenance digest %q", digest)
	}
	if strings.TrimSpace(cacheDir) == "" {
		return stableProvenanceEvidence{}, errors.New("stable provenance trust-root cache path is empty")
	}

	commit, err := resolveStableTagCommit(ctx, client, apiBaseURL, repo, rel.TagName)
	if err != nil {
		return stableProvenanceEvidence{}, fmt.Errorf("resolve stable source tag: %w", err)
	}

	workflowURI := stableWorkflowURI(repo, rel.TagName)
	repoURI := "https://github.com/" + repo
	identity, err := stableCertificateIdentity(owner, repoURI, workflowURI, rel.TagName, commit)
	if err != nil {
		return stableProvenanceEvidence{}, fmt.Errorf("build stable provenance identity: %w", err)
	}
	digestBytes, err := hex.DecodeString(digest)
	if err != nil {
		return stableProvenanceEvidence{}, fmt.Errorf("decode stable provenance digest: %w", err)
	}
	policy := sigverify.NewPolicy(
		sigverify.WithArtifactDigest("sha256", digestBytes),
		sigverify.WithCertificateIdentity(identity),
	)

	var verifier *sigverify.Verifier
	var lastErr error
	for page := 1; page <= maxStableAttestationPages; page++ {
		endpoint := stableAttestationsURL(apiBaseURL, repo, digest, page)
		var response stableAttestationsResponse
		if err := stableAPIGetJSON(ctx, client, endpoint, &response); err != nil {
			return stableProvenanceEvidence{}, fmt.Errorf("fetch stable provenance page %d: %w", page, err)
		}
		for i := range response.Attestations {
			record := &response.Attestations[i]
			if record.Initiator != "user" || len(record.Bundle) == 0 || bytes.Equal(record.Bundle, []byte("null")) {
				continue
			}
			var candidate bundle.Bundle
			if err := json.Unmarshal(record.Bundle, &candidate); err != nil {
				lastErr = fmt.Errorf("parse attestation bundle: %w", err)
				continue
			}
			if verifier == nil {
				verifier, err = newStableSigstoreVerifier(ctx, client, cacheDir)
				if err != nil {
					return stableProvenanceEvidence{}, fmt.Errorf("initialize stable Sigstore verifier: %w", err)
				}
			}
			result, err := verifier.Verify(&candidate, policy)
			if err != nil {
				lastErr = fmt.Errorf("verify attestation signature and identity: %w", err)
				continue
			}
			if err := verifyStableStatement(result, rel, assetName, digest, repoURI, workflowURI, rel.TagName, commit); err != nil {
				lastErr = err
				continue
			}
			return stableProvenanceEvidence{SourceCommit: commit}, nil
		}
		if len(response.Attestations) < stableAttestationPageSize {
			if lastErr != nil {
				return stableProvenanceEvidence{}, fmt.Errorf("no qualifying signed stable provenance: %w", lastErr)
			}
			return stableProvenanceEvidence{}, errors.New("no qualifying signed stable provenance found")
		}
	}
	return stableProvenanceEvidence{}, fmt.Errorf(
		"stable provenance pagination exceeded %d pages", maxStableAttestationPages)
}

func validateStableRepo(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
		!stableRepoComponentRE.MatchString(owner) || !stableRepoComponentRE.MatchString(name) {
		return "", fmt.Errorf("invalid GitHub repository %q", repo)
	}
	return owner, nil
}

func stableWorkflowURI(repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/%s@refs/tags/%s", repo, stableWorkflowPath, tag)
}

func stableCertificateIdentity(owner, repoURI, workflowURI, tag, commit string) (sigverify.CertificateIdentity, error) {
	san, err := sigverify.NewSANMatcher(workflowURI, "")
	if err != nil {
		return sigverify.CertificateIdentity{}, err
	}
	issuer, err := sigverify.NewIssuerMatcher(stableGitHubOIDCIssuer, "")
	if err != nil {
		return sigverify.CertificateIdentity{}, err
	}
	return sigverify.NewCertificateIdentity(san, issuer, certificate.Extensions{
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
	})
}

func newStableSigstoreVerifier(ctx context.Context, client *http.Client, cacheDir string) (*sigverify.Verifier, error) {
	tufCache := filepath.Join(cacheDir, "tuf-public-good")
	if err := os.MkdirAll(tufCache, 0700); err != nil {
		return nil, fmt.Errorf("create TUF cache: %w", err)
	}

	options := tuf.DefaultOptions()
	options.CachePath = tufCache
	options.CacheValidity = stableTrustedRootCacheDays
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPClient(stableContextHTTPClient(ctx, client))
	f.SetHTTPUserAgent(userAgent)
	options.WithFetcher(f)

	trustedRoot, err := root.FetchTrustedRootWithOptions(options)
	if err != nil {
		return nil, fmt.Errorf("load public-good trusted root with TUF: %w", err)
	}
	verifier, err := sigverify.NewVerifier(
		trustedRoot,
		sigverify.WithSignedCertificateTimestamps(1),
		sigverify.WithTransparencyLog(1),
		sigverify.WithObserverTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("create public-good verifier: %w", err)
	}
	return verifier, nil
}

type stableContextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t stableContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.Clone(t.ctx))
}

func stableContextHTTPClient(ctx context.Context, client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}
	copyClient := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copyClient.Transport = stableContextTransport{ctx: ctx, base: transport}
	return &copyClient
}

func resolveStableTagCommit(ctx context.Context, client *http.Client, apiBaseURL, repo, tag string) (string, error) {
	if _, err := validateStableRepo(repo); err != nil {
		return "", err
	}
	if _, _, ok := parseStableTag(tag); !ok {
		return "", fmt.Errorf("invalid canonical stable tag %q", tag)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/git/ref/tags/%s",
		strings.TrimRight(apiBaseURL, "/"), repo, url.PathEscape(tag))
	var object stableGitObject
	if err := stableAPIGetJSON(ctx, client, endpoint, &object); err != nil {
		return "", err
	}

	seen := make(map[string]struct{})
	for depth := 0; depth <= maxStableGitTagIndirections; depth++ {
		sha := object.Object.SHA
		if !gitCommitRE.MatchString(sha) {
			return "", fmt.Errorf("tag object has invalid Git SHA %q", sha)
		}
		switch object.Object.Type {
		case "commit":
			return sha, nil
		case "tag":
			if depth == maxStableGitTagIndirections {
				return "", fmt.Errorf("annotated tag indirection exceeds %d", maxStableGitTagIndirections)
			}
			if _, duplicate := seen[sha]; duplicate {
				return "", fmt.Errorf("annotated tag cycle at %s", sha)
			}
			seen[sha] = struct{}{}
			endpoint = fmt.Sprintf("%s/repos/%s/git/tags/%s",
				strings.TrimRight(apiBaseURL, "/"), repo, sha)
			object = stableGitObject{}
			if err := stableAPIGetJSON(ctx, client, endpoint, &object); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("tag resolves to unsupported Git object type %q", object.Object.Type)
		}
	}
	return "", errors.New("unreachable stable tag resolution state")
}

func stableAttestationsURL(apiBaseURL, repo, digest string, page int) string {
	query := url.Values{}
	query.Set("page", strconv.Itoa(page))
	query.Set("per_page", strconv.Itoa(stableAttestationPageSize))
	query.Set("predicate_type", stableSLSAPredicateType)
	return fmt.Sprintf("%s/repos/%s/attestations/%s?%s",
		strings.TrimRight(apiBaseURL, "/"), repo, url.PathEscape("sha256:"+digest), query.Encode())
}

func stableAPIGetJSON(ctx context.Context, client *http.Client, endpoint string, dst any) error {
	if client == nil {
		return errors.New("stable API HTTP client is nil")
	}
	requestCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d for %s", response.StatusCode, endpoint)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxStableAPIResponseSize+1))
	if err != nil {
		return err
	}
	if len(body) > maxStableAPIResponseSize {
		return fmt.Errorf("API response exceeds %d bytes", maxStableAPIResponseSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func verifyStableStatement(
	result *sigverify.VerificationResult,
	rel *release,
	assetName, digest, repoURI, workflowURI, tag, commit string,
) error {
	if result == nil || result.Statement == nil {
		return errors.New("verified stable provenance has no in-toto statement")
	}
	body, err := protojson.Marshal(result.Statement)
	if err != nil {
		return fmt.Errorf("encode verified SLSA statement: %w", err)
	}
	var claims stableStatementClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		return fmt.Errorf("decode verified SLSA statement: %w", err)
	}
	return verifyStableStatementClaims(claims, rel, assetName, digest, repoURI, workflowURI, tag, commit)
}

func verifyStableStatementClaims(
	claims stableStatementClaims,
	rel *release,
	assetName, digest, repoURI, workflowURI, tag, commit string,
) error {
	if claims.Type != stableInTotoStatementType || claims.PredicateType != stableSLSAPredicateType {
		return errors.New("stable provenance statement type/predicate mismatch")
	}
	expectedSubjects := make(map[string]string, len(stableBinaryAssetNames))
	for _, name := range stableBinaryAssetNames {
		a, err := findUniqueAsset(rel, name)
		if err != nil {
			return err
		}
		if a == nil || !strings.HasPrefix(a.Digest, "sha256:") {
			return fmt.Errorf("stable provenance release has no digest for %s", name)
		}
		expectedSubjects[name] = strings.TrimPrefix(a.Digest, "sha256:")
	}
	if expectedSubjects[assetName] != digest {
		return errors.New("stable provenance requested asset/digest does not match Release metadata")
	}
	if len(claims.Subject) != len(expectedSubjects) {
		return fmt.Errorf("stable provenance has %d subjects, want exactly %d", len(claims.Subject), len(expectedSubjects))
	}
	for _, subject := range claims.Subject {
		want, ok := expectedSubjects[subject.Name]
		if !ok || len(subject.Digest) != 1 || subject.Digest["sha256"] != want {
			return fmt.Errorf("stable provenance has unexpected subject %q", subject.Name)
		}
		delete(expectedSubjects, subject.Name)
	}
	if len(expectedSubjects) != 0 {
		return errors.New("stable provenance is missing a required binary subject")
	}

	definition := claims.Predicate.BuildDefinition
	workflow := definition.ExternalParameters.Workflow
	if definition.BuildType != "https://actions.github.io/buildtypes/workflow/v1" ||
		workflow.Ref != "refs/tags/"+tag ||
		workflow.Repository != repoURI ||
		workflow.Path != stableWorkflowPath ||
		claims.Predicate.RunDetails.Builder.ID != workflowURI {
		return errors.New("stable provenance workflow claims do not match the exact stable producer")
	}
	dependencies := definition.ResolvedDependencies
	wantDependencyURI := fmt.Sprintf("git+%s@refs/tags/%s", repoURI, tag)
	if len(dependencies) != 1 || dependencies[0].URI != wantDependencyURI ||
		len(dependencies[0].Digest) != 1 || dependencies[0].Digest["gitCommit"] != commit {
		return errors.New("stable provenance source dependency does not match the resolved tag commit")
	}
	return nil
}
