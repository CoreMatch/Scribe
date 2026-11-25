package race

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestEvaluateADRMatrix(t *testing.T) {
	profile := success(true)
	noProfile := success(false)
	forbidden := Result{StatusCode: http.StatusForbidden}
	unavailable := Result{Err: errors.New("upstream unavailable")}
	malformed := Result{StatusCode: http.StatusOK, Body: []byte(`not-json`)}

	tests := []struct {
		name    string
		target  Result
		legacy  Result
		intent  Intent
		outcome Outcome
		reason  Reason
	}{
		{name: "new playable profile wins", target: profile, legacy: forbidden, outcome: NewWins},
		{name: "new playable profile wins despite legacy failure", target: profile, legacy: unavailable, outcome: NewWins},
		{name: "reuse core account", target: noProfile, legacy: profile, intent: IntentImport, outcome: MigrateReuseCore},
		{name: "create core account", target: forbidden, legacy: profile, outcome: MigrateCreateCore},
		{name: "target timeout blocks", target: unavailable, legacy: profile, outcome: BlockRetry, reason: ReasonTargetStateUnknown},
		{name: "target server error blocks", target: Result{StatusCode: http.StatusBadGateway}, legacy: profile, outcome: BlockRetry, reason: ReasonTargetStateUnknown},
		{name: "malformed target blocks", target: malformed, legacy: profile, outcome: BlockRetry, reason: ReasonMalformedTargetResponse},
		{name: "both reject credentials", target: forbidden, legacy: forbidden, outcome: NoBinding, reason: ReasonNoPlayableLegacyProfile},
		{name: "legacy has no playable profile", target: forbidden, legacy: noProfile, outcome: NoBinding, reason: ReasonNoPlayableLegacyProfile},
		{name: "import intent is required", target: noProfile, legacy: profile, outcome: BlockRetry, reason: ReasonMigrationIntentRequired},
		{name: "opt out fails closed", target: noProfile, legacy: profile, intent: IntentOptOut, outcome: BlockRetry, reason: ReasonMigrationOptedOut},
		{name: "malformed legacy blocks", target: forbidden, legacy: malformed, outcome: BlockRetry, reason: ReasonMalformedLegacyResponse},
		{name: "legacy unavailable blocks", target: forbidden, legacy: unavailable, outcome: BlockRetry, reason: ReasonLegacyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := Evaluate(tt.target, tt.legacy, tt.intent)
			if decision.Outcome != tt.outcome || decision.Reason != tt.reason {
				t.Fatalf("Evaluate() = %+v, want outcome=%s reason=%s", decision, tt.outcome, tt.reason)
			}
		})
	}
}

func TestEvaluateNeverMigratesWithoutLegacyProfile(t *testing.T) {
	for _, target := range []Result{success(false), {StatusCode: http.StatusForbidden}} {
		for _, legacy := range []Result{success(false), {StatusCode: http.StatusForbidden}, {StatusCode: http.StatusNoContent}} {
			decision := Evaluate(target, legacy, IntentImport)
			if decision.Outcome == MigrateReuseCore || decision.Outcome == MigrateCreateCore {
				t.Fatalf("unexpected migration decision: %+v", decision)
			}
		}
	}
}

func TestRunStartsBothUpstreamsConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	fn := func(context.Context) Result {
		started <- struct{}{}
		<-release
		return success(true)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultCh := make(chan Decision, 1)
	go func() { resultCh <- Run(ctx, IntentUnspecified, fn, fn) }()

	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("both upstreams did not start concurrently")
		}
	}
	close(release)
	select {
	case decision := <-resultCh:
		if decision.Outcome != NewWins {
			t.Fatalf("Run() = %+v, want NEW_WINS", decision)
		}
	case <-ctx.Done():
		t.Fatal("Run() did not complete")
	}
}

func TestRunReturnsImmediatelyWhenTargetHasPlayableProfile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	legacyCanceled := make(chan struct{})
	var once sync.Once
	decision := Run(ctx, IntentUnspecified,
		func(context.Context) Result { return success(true) },
		func(ctx context.Context) Result {
			<-ctx.Done()
			once.Do(func() { close(legacyCanceled) })
			return Result{Err: ctx.Err()}
		},
	)
	if decision.Outcome != NewWins {
		t.Fatalf("Run() = %+v, want NEW_WINS", decision)
	}
	select {
	case <-legacyCanceled:
	case <-time.After(time.Second):
		t.Fatal("legacy request was not canceled after target won")
	}
}

func TestRunBlocksOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decision := Run(ctx, IntentImport,
		func(context.Context) Result { return success(true) },
		func(context.Context) Result { return success(true) },
	)
	if decision.Outcome != BlockRetry || decision.Reason != ReasonTargetStateUnknown {
		t.Fatalf("Run() = %+v, want BLOCK_RETRY", decision)
	}
}

func success(withProfile bool) Result {
	body := `{"accessToken":"access","clientToken":"client"}`
	if withProfile {
		body = `{"accessToken":"access","clientToken":"client","selectedProfile":{"id":"uuid","name":"Player"}}`
	}
	return Result{StatusCode: http.StatusOK, Body: []byte(body)}
}
