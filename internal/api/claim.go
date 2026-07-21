package api

// ClaimStatus is a stable, privacy-safe classification of the authoritative
// result of a side-effecting Twitch claim mutation (a channel-points bonus
// chest or a drop reward). It deliberately carries ONLY the outcome class —
// never the raw response, the claim/instance ID, the OAuth token, or any
// header — so it is safe to log and to assert on in tests.
//
// Both claim paths funnel their responses through this one contract
// (classifyCommunityPointsClaim, classifyDropClaim) so the "what counts as an
// accepted claim" decision cannot drift between the two mutations.
type ClaimStatus string

const (
	// ClaimStatusAccepted is a fresh, authoritative acceptance: Twitch returned
	// the mutation's business-result node and did not reject it.
	ClaimStatusAccepted ClaimStatus = "accepted"

	// ClaimStatusAlreadyClaimed is an authoritative already-claimed/-completed
	// response. It is a reconciled success — local state should converge to
	// claimed — but it is NOT a fresh claim, so callers must not emit a second
	// user-facing success event for it.
	ClaimStatusAlreadyClaimed ClaimStatus = "already_claimed"

	// ClaimStatusRejected is an authoritative rejection carried inside the
	// mutation payload (an embedded error node, or a non-accepting status).
	// Retrying the same request will not change it.
	ClaimStatusRejected ClaimStatus = "rejected"

	// ClaimStatusMissingData means the response carried no usable top-level
	// `data` object (absent, null, or the wrong type).
	ClaimStatusMissingData ClaimStatus = "missing_data"

	// ClaimStatusMissingResult means `data` was present but the mutation's
	// business-result node was absent.
	ClaimStatusMissingResult ClaimStatus = "missing_result"

	// ClaimStatusNullResult means the business-result node was present but null.
	ClaimStatusNullResult ClaimStatus = "null_result"

	// ClaimStatusMalformed means the business-result node was present but not the
	// expected shape (e.g. not an object, or missing its status field).
	ClaimStatusMalformed ClaimStatus = "malformed_result"

	// ClaimStatusGraphQLError means the response carried a top-level GraphQL
	// `errors` array, so it returned no authoritative data.
	ClaimStatusGraphQLError ClaimStatus = "graphql_error"
)

// Accepted reports whether the mutation was authoritatively accepted — either a
// fresh claim or an idempotent already-claimed reconciliation. Only these two
// outcomes may mark local state as claimed.
func (s ClaimStatus) Accepted() bool {
	return s == ClaimStatusAccepted || s == ClaimStatusAlreadyClaimed
}

// Fresh reports whether this outcome is a brand-new claim (as opposed to an
// already-claimed reconciliation). A user-facing success event must be emitted
// only for a Fresh acceptance, so repeated reconciliation of an already-claimed
// item can never produce duplicate success events.
func (s ClaimStatus) Fresh() bool {
	return s == ClaimStatusAccepted
}

// Retryable reports whether a non-accepted outcome could plausibly succeed on a
// later attempt through the existing bounded retry paths (PubSub re-delivery /
// the polling fallback for bonuses, the next inventory sync for drops). An
// authoritative rejection is terminal and never retryable; a
// missing/null/malformed/transient-shaped response is.
func (s ClaimStatus) Retryable() bool {
	switch s {
	case ClaimStatusAccepted, ClaimStatusAlreadyClaimed, ClaimStatusRejected:
		return false
	default:
		return true
	}
}

// classifyCommunityPointsClaim inspects a ClaimCommunityPoints response and
// returns the authoritative outcome without exposing any payload. The mutation's
// business-result node is `data.claimCommunityPoints`.
//
// Because this is a side-effecting mutation, acceptance must be POSITIVELY
// established, not merely inferred from the absence of a rejection — the burden
// of proof for "success" lies on the success classification. No repository
// fixture, persisted-query selection set, or captured response confirms the
// exact success shape (the operation is sent as an Automatic Persisted Query, so
// only its hash is in the tree), so the parser is fail-closed:
//
//   - a non-null embedded `error` OBJECT is an authoritative rejection;
//   - a non-null `error` of any other type is a malformed shape and is NOT read
//     as success;
//   - the business-result node must carry positive evidence of a result — at
//     least one field (a returned business-result field, or an explicit
//     `error: null` "no error" marker). An EMPTY object `{}` carries no such
//     evidence and is treated as malformed, mirroring how classifyDropClaim
//     already fail-closes a status-less `claimDropRewards: {}`.
//
// No specific inner success field is required (none is confirmed), so a non-empty
// error-free node is accepted; only the evidence-free empty object is rejected.
func classifyCommunityPointsClaim(resp map[string]interface{}) ClaimStatus {
	if hasTopLevelGQLErrors(resp) {
		return ClaimStatusGraphQLError
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok || data == nil {
		return ClaimStatusMissingData
	}
	node, present := data["claimCommunityPoints"]
	if !present {
		return ClaimStatusMissingResult
	}
	if node == nil {
		return ClaimStatusNullResult
	}
	payload, ok := node.(map[string]interface{})
	if !ok {
		return ClaimStatusMalformed
	}

	// A non-null error is authoritative: an error object is a rejection; a
	// non-null error of any other type is malformed and must never be read as
	// success.
	if errVal, hasErr := payload["error"]; hasErr && errVal != nil {
		if _, isObj := errVal.(map[string]interface{}); isObj {
			return ClaimStatusRejected
		}
		return ClaimStatusMalformed
	}

	// No non-null error. Acceptance still requires positive evidence of a
	// business result; an empty object provides none, so it is fail-closed.
	if len(payload) == 0 {
		return ClaimStatusMalformed
	}
	return ClaimStatusAccepted
}

// classifyDropClaim inspects a DropsPage_ClaimDropRewards response and returns
// the authoritative outcome. The business-result node is `data.claimDropRewards`,
// whose `status` string is Twitch's authoritative verdict. The two accepted
// statuses are preserved exactly as before: ELIGIBLE_FOR_ALL is a fresh claim,
// DROP_INSTANCE_ALREADY_CLAIMED is an idempotent already-claimed reconciliation.
// Any other status is an authoritative rejection; a missing/null/malformed node
// is fail-closed (never treated as success).
func classifyDropClaim(resp map[string]interface{}) ClaimStatus {
	if hasTopLevelGQLErrors(resp) {
		return ClaimStatusGraphQLError
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok || data == nil {
		return ClaimStatusMissingData
	}
	node, present := data["claimDropRewards"]
	if !present {
		return ClaimStatusMissingResult
	}
	if node == nil {
		return ClaimStatusNullResult
	}
	claimRewards, ok := node.(map[string]interface{})
	if !ok {
		return ClaimStatusMalformed
	}
	status, ok := claimRewards["status"].(string)
	if !ok {
		return ClaimStatusMalformed
	}
	switch status {
	case "ELIGIBLE_FOR_ALL":
		return ClaimStatusAccepted
	case "DROP_INSTANCE_ALREADY_CLAIMED":
		return ClaimStatusAlreadyClaimed
	default:
		return ClaimStatusRejected
	}
}
