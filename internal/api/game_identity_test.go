package api

import (
	"io"
	"net/http"
	"testing"
)

// gameRedirectBody is a DirectoryGameRedirect success body carrying game.id +
// game.slug, the shape GetGameIdentity parses.
func gameRedirectBody(id, slug string) string {
	return `{"data":{"game":{"id":"` + id + `","slug":"` + slug + `"}}}`
}

// T-H1: the resolver returns both the opaque game ID and the slug.
func TestGetGameIdentityReturnsIDAndSlug(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, gameRedirectBody("27546", "world-of-tanks"))
	})
	got, err := c.GetGameIdentity("World of Tanks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "27546" || got.Slug != "world-of-tanks" {
		t.Fatalf("expected {27546, world-of-tanks}, got %+v", got)
	}
}

// T-H3 + T-H4: the ID is opaque — a non-numeric value is returned verbatim and
// its case is preserved byte-for-byte (no lowercasing, no transformation).
func TestGetGameIdentityIDIsOpaqueAndCasePreserved(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, gameRedirectBody("game-WoT-42", "slug-x"))
	})
	got, err := c.GetGameIdentity("World of Tanks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "game-WoT-42" {
		t.Fatalf("opaque ID must be returned verbatim (case preserved), got %q", got.ID)
	}
}

// T-H5: a blank (or whitespace-only) name makes no network call.
func TestGetGameIdentityEmptyNameNoNetwork(t *testing.T) {
	called := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, gameRedirectBody("1", "x"))
	})
	got, err := c.GetGameIdentity("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (GameIdentity{}) {
		t.Fatalf("blank name must yield a zero identity, got %+v", got)
	}
	if called {
		t.Fatal("blank name must not hit the network")
	}
}

// T-H6: data.game == null → zero identity, no error (a game Twitch doesn't know
// is not an error, and must not produce a fake ID).
func TestGetGameIdentityUnknownGameZeroIdentity(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"game":null}}`)
	})
	got, err := c.GetGameIdentity("No Such Game")
	if err != nil {
		t.Fatalf("unknown game must not be an error, got %v", err)
	}
	if got != (GameIdentity{}) {
		t.Fatalf("unknown game must yield a zero identity, got %+v", got)
	}
}

// T-H7: GQL errors surface as an error (kept distinct from "unknown game"), and
// never as a fabricated identity.
func TestGetGameIdentityGQLErrorsReturnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"service error"}]}`)
	})
	got, err := c.GetGameIdentity("World of Tanks")
	if err == nil {
		t.Fatal("GQL errors must surface as an error")
	}
	if got.ID != "" || got.Slug != "" {
		t.Fatalf("an errored lookup must not fabricate an identity, got %+v", got)
	}
}

// A response that carries BOTH data.game=null AND a non-empty errors array is a
// failure, not "unknown game": the errors are checked before data, so this
// surfaces an error rather than a zero identity.
func TestGetGameIdentityErrorsBesideNullGameReturnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"game":null},"errors":[{"message":"service error"}]}`)
	})
	got, err := c.GetGameIdentity("World of Tanks")
	if err == nil {
		t.Fatal("errors beside a null game must surface as an error, not unknown-game")
	}
	if got.ID != "" || got.Slug != "" {
		t.Fatalf("an errored lookup must not fabricate an identity, got %+v", got)
	}
}

// T-H2: GetGameSlug reuses the shared resolver and preserves discovery's
// behaviour — it returns the slug from the same response.
func TestGetGameSlugReusesResolver(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, gameRedirectBody("27546", "world-of-tanks"))
	})
	slug, err := c.GetGameSlug("World of Tanks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slug != "world-of-tanks" {
		t.Fatalf("expected slug world-of-tanks, got %q", slug)
	}
}
