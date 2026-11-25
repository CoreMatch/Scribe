package race

import (
	"context"
	"net/http"

	"github.com/corematch/scribe/internal/protocol"
)

type Intent string

const (
	IntentUnspecified Intent = ""
	IntentImport      Intent = "import"
	IntentOptOut      Intent = "opt_out"
)

type Outcome string

const (
	NewWins           Outcome = "NEW_WINS"
	MigrateReuseCore  Outcome = "MIGRATE_REUSE_CORE"
	MigrateCreateCore Outcome = "MIGRATE_CREATE_CORE"
	BlockRetry        Outcome = "BLOCK_RETRY"
	NoBinding         Outcome = "NO_BINDING"
)

type Reason string

const (
	ReasonNone                    Reason = ""
	ReasonTargetStateUnknown      Reason = "target_state_unknown"
	ReasonMigrationIntentRequired Reason = "migration_intent_required"
	ReasonMigrationOptedOut       Reason = "migration_opted_out"
	ReasonNoPlayableLegacyProfile Reason = "no_playable_legacy_profile"
	ReasonInvalidCredentials      Reason = "invalid_credentials"
	ReasonMalformedTargetResponse Reason = "malformed_target_response"
	ReasonMalformedLegacyResponse Reason = "malformed_legacy_response"
	ReasonLegacyUnavailable       Reason = "legacy_unavailable"
	ReasonAuthenticationFailed    Reason = "authentication_failed"
)

type Result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Err        error
}

type Decision struct {
	Outcome Outcome
	Reason  Reason
	Target  Result
	Legacy  Result
}

type AuthenticationFunc func(context.Context) Result

func Evaluate(target, legacy Result, intent Intent) Decision {
	targetKind := classifyTarget(target)
	legacyKind := classifyLegacy(legacy)

	switch targetKind {
	case targetSuccessWithProfile:
		return Decision{Outcome: NewWins}
	case targetInvalidCredentials:
		if legacyKind == legacySuccessWithProfile {
			return Decision{Outcome: MigrateCreateCore}
		}
		if legacyKind == legacyMalformed {
			return Decision{Outcome: BlockRetry, Reason: ReasonMalformedLegacyResponse}
		}
		if legacyKind == legacyUnavailable {
			return Decision{Outcome: BlockRetry, Reason: ReasonLegacyUnavailable}
		}
		return Decision{Outcome: NoBinding, Reason: ReasonNoPlayableLegacyProfile}
	case targetSuccessWithoutProfile:
		if legacyKind != legacySuccessWithProfile {
			if legacyKind == legacyMalformed {
				return Decision{Outcome: BlockRetry, Reason: ReasonMalformedLegacyResponse}
			}
			if legacyKind == legacyUnavailable {
				return Decision{Outcome: BlockRetry, Reason: ReasonLegacyUnavailable}
			}
			return Decision{Outcome: NoBinding, Reason: ReasonNoPlayableLegacyProfile}
		}
		switch intent {
		case IntentImport:
			return Decision{Outcome: MigrateReuseCore}
		case IntentOptOut:
			return Decision{Outcome: BlockRetry, Reason: ReasonMigrationOptedOut}
		default:
			return Decision{Outcome: BlockRetry, Reason: ReasonMigrationIntentRequired}
		}
	case targetMalformed:
		return Decision{Outcome: BlockRetry, Reason: ReasonMalformedTargetResponse}
	default:
		return Decision{Outcome: BlockRetry, Reason: ReasonTargetStateUnknown}
	}
}

func Run(ctx context.Context, intent Intent, target, legacy AuthenticationFunc) Decision {
	if ctx.Err() != nil {
		return Decision{Outcome: BlockRetry, Reason: ReasonTargetStateUnknown}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	targetCh := make(chan Result, 1)
	legacyCh := make(chan Result, 1)
	go func() { targetCh <- target(ctx) }()
	go func() { legacyCh <- legacy(ctx) }()

	var targetResult, legacyResult Result
	select {
	case <-ctx.Done():
		return Decision{Outcome: BlockRetry, Reason: ReasonTargetStateUnknown}
	case targetResult = <-targetCh:
	}
	if classifyTarget(targetResult) == targetSuccessWithProfile {
		return Decision{Outcome: NewWins, Target: targetResult}
	}
	select {
	case <-ctx.Done():
		return Decision{Outcome: BlockRetry, Reason: ReasonTargetStateUnknown}
	case legacyResult = <-legacyCh:
	}
	decision := Evaluate(targetResult, legacyResult, intent)
	decision.Target = targetResult
	decision.Legacy = legacyResult
	return decision
}

type targetKind int

const (
	targetUnavailable targetKind = iota
	targetInvalidCredentials
	targetSuccessWithoutProfile
	targetSuccessWithProfile
	targetMalformed
)

type legacyKind int

const (
	legacyFailure legacyKind = iota
	legacyUnavailable
	legacySuccessWithoutProfile
	legacySuccessWithProfile
	legacyMalformed
)

func classifyTarget(result Result) targetKind {
	if result.Err != nil {
		return targetUnavailable
	}
	if result.StatusCode == http.StatusForbidden {
		return targetInvalidCredentials
	}
	if result.StatusCode != http.StatusOK {
		if result.StatusCode == 0 || result.StatusCode >= http.StatusInternalServerError {
			return targetUnavailable
		}
		return targetMalformed
	}
	response, err := protocol.DecodeAuthenticateResponse(result.Body)
	if err != nil {
		return targetMalformed
	}
	if response.SelectedProfile != nil {
		return targetSuccessWithProfile
	}
	return targetSuccessWithoutProfile
}

func classifyLegacy(result Result) legacyKind {
	if result.Err != nil {
		return legacyUnavailable
	}
	if result.StatusCode == http.StatusForbidden {
		return legacyFailure
	}
	if result.StatusCode != http.StatusOK {
		if result.StatusCode == 0 || result.StatusCode >= http.StatusInternalServerError {
			return legacyUnavailable
		}
		return legacyFailure
	}
	response, err := protocol.DecodeAuthenticateResponse(result.Body)
	if err != nil {
		return legacyMalformed
	}
	if response.SelectedProfile != nil {
		return legacySuccessWithProfile
	}
	return legacySuccessWithoutProfile
}
