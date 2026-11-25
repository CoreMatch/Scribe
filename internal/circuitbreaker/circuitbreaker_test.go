package circuitbreaker

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corematch/scribe/internal/race"
	"github.com/sony/gobreaker"
)

func TestBreakerOpensAfterConsecutiveServerFailures(t *testing.T) {
	var calls atomic.Int32
	fn := func(context.Context) race.Result {
		calls.Add(1)
		return race.Result{StatusCode: http.StatusBadGateway}
	}
	breaker, err := Wrap(fn, Settings{FailureThreshold: 2, OpenTimeout: time.Minute, HalfOpenMaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		breaker.Authenticate(context.Background())
	}
	if breaker.State() != gobreaker.StateOpen {
		t.Fatalf("state = %s, want open", breaker.State())
	}
	result := breaker.Authenticate(context.Background())
	if result.Err == nil || calls.Load() != 2 {
		t.Fatalf("open breaker result=%+v calls=%d", result, calls.Load())
	}
}

func TestBreakerDoesNotTripOnInvalidCredentials(t *testing.T) {
	fn := func(context.Context) race.Result {
		return race.Result{StatusCode: http.StatusForbidden, Body: []byte(`{"error":"ForbiddenOperationException"}`)}
	}
	breaker, err := Wrap(fn, Settings{FailureThreshold: 2, OpenTimeout: time.Minute, HalfOpenMaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		result := breaker.Authenticate(context.Background())
		if result.StatusCode != http.StatusForbidden || result.Err != nil {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if breaker.State() != gobreaker.StateClosed {
		t.Fatalf("state = %s, want closed", breaker.State())
	}
}

func TestBreakerTripsOnTransportAndRateLimitFailures(t *testing.T) {
	tests := []struct {
		name   string
		result race.Result
	}{
		{name: "transport", result: race.Result{Err: errors.New("connection refused")}},
		{name: "rate limit", result: race.Result{StatusCode: http.StatusTooManyRequests}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			breaker, err := Wrap(func(context.Context) race.Result { return tt.result }, Settings{FailureThreshold: 1, OpenTimeout: time.Minute, HalfOpenMaxRequests: 1})
			if err != nil {
				t.Fatal(err)
			}
			breaker.Authenticate(context.Background())
			if breaker.State() != gobreaker.StateOpen {
				t.Fatalf("state = %s, want open", breaker.State())
			}
		})
	}
}

func TestBreakerLeavesMalformedSuccessToRaceEvaluator(t *testing.T) {
	breaker, err := Wrap(func(context.Context) race.Result {
		return race.Result{StatusCode: http.StatusOK, Body: []byte(`not-json`)}
	}, Settings{FailureThreshold: 1, OpenTimeout: time.Minute, HalfOpenMaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	result := breaker.Authenticate(context.Background())
	if result.Err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("unexpected result: %+v", result)
	}
	if breaker.State() != gobreaker.StateClosed {
		t.Fatalf("state = %s, want closed", breaker.State())
	}
}

func TestBreakerHalfOpenRecovery(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	breaker, err := Wrap(func(context.Context) race.Result {
		if fail.Load() {
			return race.Result{StatusCode: http.StatusServiceUnavailable}
		}
		return race.Result{StatusCode: http.StatusForbidden}
	}, Settings{FailureThreshold: 1, OpenTimeout: 10 * time.Millisecond, HalfOpenMaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	breaker.Authenticate(context.Background())
	if breaker.State() != gobreaker.StateOpen {
		t.Fatal("breaker did not open")
	}
	time.Sleep(20 * time.Millisecond)
	fail.Store(false)
	result := breaker.Authenticate(context.Background())
	if result.Err != nil || result.StatusCode != http.StatusForbidden {
		t.Fatalf("recovery result = %+v", result)
	}
	if breaker.State() != gobreaker.StateClosed {
		t.Fatalf("state = %s, want closed", breaker.State())
	}
}

func TestBreakerRejectsInvalidSettings(t *testing.T) {
	fn := func(context.Context) race.Result { return race.Result{StatusCode: http.StatusForbidden} }
	for _, settings := range []Settings{
		{},
		{FailureThreshold: 1, OpenTimeout: time.Second},
		{FailureThreshold: 1, HalfOpenMaxRequests: 1},
	} {
		if _, err := Wrap(fn, settings); err == nil {
			t.Fatalf("Wrap(%+v) unexpectedly succeeded", settings)
		}
	}
}
