package management

import (
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// blockedModelEntry is one model that is currently not schedulable on one
// credential, with the reason and, when known, when it comes back.
//
// The pair (reason, retry_at) is the whole point: "this credential is down"
// and "this credential's claude-opus is cooling until 14:20 while everything
// else works" call for completely different responses from an operator, and
// only the second is actionable.
type blockedModelEntry struct {
	// ID is the model identifier as the registry knows it.
	ID string `json:"id"`
	// Reason is "cooldown" (a quota/window limit) or "blocked" (anything else).
	// Cooldown is the one an operator waits out; blocked may need a human.
	Reason string `json:"reason"`
	// RetryAt is when the model becomes schedulable again, RFC3339. Omitted
	// when the block has no known expiry — an absent field is honest where a
	// zero timestamp would read as "expired".
	RetryAt string `json:"retry_at,omitempty"`
	// StatusMessage is the upstream-provided description, when one was recorded.
	StatusMessage string `json:"status_message,omitempty"`
}

// blockedModelsForAuth lists the models this credential cannot serve right now.
//
// This reads the SAME state the scheduler uses (IsModelAvailable) rather than a
// parallel interpretation of it, so what the management UI shows cannot drift
// from what routing actually does. The distinction matters: the summary tiles
// above already reduce this same walk to two booleans, and a second, more
// detailed view that disagreed with them would be worse than no detail at all.
//
// Only blocked models are returned. A model absent from the result is simply
// available — listing the healthy majority would turn a short "what is wrong"
// answer into a twelve-row wall the operator has to scan for the two bad ones.
//
// Returns nil when the credential has no models registered, which is different
// from "nothing is blocked" only in that the caller cannot tell them apart; both
// mean "nothing to report", so they share a representation.
func blockedModelsForAuth(auth *coreauth.Auth, now time.Time) []blockedModelEntry {
	if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return nil
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	if len(models) == 0 {
		return nil
	}

	blocked := make([]blockedModelEntry, 0)
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		if coreauth.IsModelAvailable(auth, model.ID, now) {
			continue
		}

		// The model state is looked up by the registry's own id. isAuthBlockedForModel
		// matches states through an unexported normaliser, so a state stored under a
		// thinking-suffixed key still answers for the bare id; this loop only needs
		// the reason and expiry, and a miss degrades to "blocked, no expiry" rather
		// than to a wrong answer.
		state := modelStateFor(auth, model.ID)

		entry := blockedModelEntry{ID: model.ID, Reason: "blocked"}
		if state != nil {
			if message := strings.TrimSpace(state.StatusMessage); message != "" {
				entry.StatusMessage = message
			}
			reason, retryAt := blockedReason(state, now)
			entry.Reason = reason
			if !retryAt.IsZero() {
				entry.RetryAt = retryAt.UTC().Format(time.RFC3339)
			}
		}
		blocked = append(blocked, entry)
	}

	// Sorted for a stable UI: without this the order follows a map iteration and
	// the same credential's list would reshuffle between polls.
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].ID < blocked[j].ID })
	return blocked
}

// blockedReason names why a model is unavailable, and when it returns.
//
// Mirrors the two conditions availabilityBlock consults: an exceeded quota is a
// cooldown the operator waits out, anything else is a block that may need a
// human. Kept separate from that function rather than threaded out of it because
// isAuthBlockedForModel is unexported and returns an internal enum; the states
// read here are the same ones it reads, so the two agree by construction.
func blockedReason(state *coreauth.ModelState, at time.Time) (string, time.Time) {
	// Checked first, matching isAuthBlockedForModel: a disabled state returns
	// immediately without consulting quota or retry times at all.
	if state.Status == coreauth.StatusDisabled {
		return "blocked", time.Time{}
	}
	if state.Quota.Exceeded {
		if state.Quota.NextRecoverAt.After(at) {
			return "cooldown", state.Quota.NextRecoverAt
		}
		if state.NextRetryAfter.After(at) {
			return "cooldown", state.NextRetryAfter
		}
		// Quota spent with no future recovery instant. Note this branch is
		// unreachable via isAuthBlockedForModel: availabilityBlock treats a
		// recovery time that has already elapsed as AVAILABLE, so such a model
		// is filtered out before it reaches here. Kept as the honest answer for
		// a state that is somehow blocked anyway — a spent window, not a fault.
		return "cooldown", time.Time{}
	}
	if state.Unavailable && state.NextRetryAfter.After(at) {
		return "blocked", state.NextRetryAfter
	}
	return "blocked", time.Time{}
}

// modelStateFor finds the ModelStates entry for a model id.
//
// Exact match only, because that is what the scheduler does: canonicalModelKey
// trims and strips a thinking suffix but does NOT fold case, so a state keyed
// "GEMINI-A" genuinely does not serve a lookup for "gemini-a" and the model
// really is blocked. Folding case here would make this view report a model as
// blocked-with-a-reason in cases routing treats as unmatched — the one kind of
// disagreement this field must not have.
func modelStateFor(auth *coreauth.Auth, modelID string) *coreauth.ModelState {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	if state, ok := auth.ModelStates[modelID]; ok && state != nil {
		return state
	}
	return nil
}
