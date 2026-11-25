package circuitbreaker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/corematch/scribe/internal/protocol"
	"github.com/corematch/scribe/internal/race"
	"github.com/sony/gobreaker"
)

type Settings struct {
	FailureThreshold    uint32
	OpenTimeout         time.Duration
	HalfOpenMaxRequests uint32
}

var errUpstreamUnavailable = errors.New("upstream unavailable")

func Wrap(fn race.AuthenticationFunc, settings Settings) (*Breaker, error) {
	if fn == nil {
		return nil, errors.New("authentication function is required")
	}
	if settings.FailureThreshold == 0 || settings.OpenTimeout <= 0 || settings.HalfOpenMaxRequests == 0 {
		return nil, errors.New("circuit breaker settings must be positive")
	}
	breaker := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "yggdrasil-authentication",
		MaxRequests: settings.HalfOpenMaxRequests,
		Interval:    0,
		Timeout:     settings.OpenTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= settings.FailureThreshold
		},
	})
	wrapped := &Breaker{breaker: breaker}
	wrapped.fn = func(ctx context.Context) race.Result {
		value, err := breaker.Execute(func() (any, error) {
			result := fn(ctx)
			if result.Err != nil {
				return result, errUpstreamUnavailable
			}
			if result.StatusCode == 0 || result.StatusCode == http.StatusTooManyRequests || result.StatusCode >= http.StatusInternalServerError {
				return result, errUpstreamUnavailable
			}
			if result.StatusCode == http.StatusOK {
				if _, err := protocol.DecodeAuthenticateResponse(result.Body); err != nil {
					return result, nil
				}
			}
			return result, nil
		})
		if value == nil {
			return race.Result{Err: err}
		}
		result := value.(race.Result)
		if err != nil {
			result.Err = err
		}
		return result
	}
	return wrapped, nil
}

type Breaker struct {
	breaker *gobreaker.CircuitBreaker
	fn      race.AuthenticationFunc
}

func (b *Breaker) Authenticate(ctx context.Context) race.Result {
	return b.fn(ctx)
}

func (b *Breaker) State() gobreaker.State {
	return b.breaker.State()
}
