package api

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// C7.3/C7.4: a 200 body whose errors[].message merely CONTAINS the word
// "unauthorized" (or is a permission/scope/business rejection) is NOT an
// authoritative token rejection — no recovery, no replay, no reauth handler.
func TestUnrelatedUnauthorizedSubstringDoesNotTriggerRecovery(t *testing.T) {
	bodies := map[string]string{
		"entitlement":   `{"errors":[{"message":"unauthorized entitlement for this drop"}],"data":null}`,
		"not-authz-op":  `{"errors":[{"message":"you are not authorized for this operation"}],"data":null}`,
		"business-perm": `{"errors":[{"message":"permission denied: missing scope"}],"data":null}`,
		// Only the byte-exact evidenced "Unauthorized" is trusted; a case
		// variant is not proof of a token rejection.
		"case-variant": `{"errors":[{"message":"unauthorized"}],"data":null}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			requests := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				_, _ = io.WriteString(w, body)
			})
			recoveries := installCountingRecoverFn(c)
			fired := installAuthErrorCounter(c)

			op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
			_, err := c.PostGQL(op)
			if errors.Is(err, ErrUnauthorized) {
				t.Fatalf("business error misclassified as ErrUnauthorized")
			}
			if recoveries.Load() != 0 {
				t.Fatalf("business error triggered %d auth recoveries (refresh-token spend!)", recoveries.Load())
			}
			if fired.Load() != 0 {
				t.Fatalf("business error fired the reauth handler")
			}
			if requests != 1 {
				t.Fatalf("business error caused %d requests, want 1 (no replay)", requests)
			}
		})
	}
}

// C7.6: repeated business errors never build up into a recovery storm.
func TestRepeatedBusinessErrorsNoRecoveryStorm(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"unauthorized entitlement"}],"data":null}`)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	for range 5 {
		_, _ = c.PostGQL(op)
	}
	if recoveries.Load() != 0 {
		t.Fatalf("recovery storm: %d recoveries from pure business errors", recoveries.Load())
	}
}

// C7.2: the one repo-evidenced 200-body auth shape (top-level
// {"error":"Unauthorized"}) still recovers exactly once.
func TestConfirmedTopLevelUnauthorizedStillRecovers(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			_, _ = io.WriteString(w, `{"error":"Unauthorized","status":401,"message":"..."}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{}}`)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("recovered call failed: %v", err)
	}
	if recoveries.Load() != 1 || requests != 2 {
		t.Fatalf("confirmed shape: recoveries=%d requests=%d, want 1/2", recoveries.Load(), requests)
	}
}
