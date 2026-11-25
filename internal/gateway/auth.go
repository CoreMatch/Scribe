package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/protocol"
	"github.com/corematch/scribe/internal/race"
)

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) {
	body, ok := g.readRequestBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeAuthenticateRequest(body)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "IllegalArgumentException", "Bad request.")
		return
	}
	identifier, err := protocol.NormalizeIdentifier(request.Username)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "IllegalArgumentException", "Bad request.")
		return
	}
	existing, err := g.store.GetBinding(r.Context(), identifier)
	if err == nil {
		result := g.execute(r.Context(), existing.Winner, r, body)
		if result.Err == nil && result.StatusCode == http.StatusOK {
			g.completeAuthentication(w, r, result, identifier, existing.Winner, existing.MigrationID, false)
			return
		}
		g.writeResult(w, result)
		return
	}
	if !errors.Is(err, binding.ErrNotFound) {
		g.log.Error("binding lookup failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication service temporarily unavailable.")
		return
	}

	decision := race.Run(r.Context(), g.migrationIntent,
		func(ctx context.Context) race.Result { return g.execute(ctx, binding.Armorial, r, body) },
		func(ctx context.Context) race.Result { return g.execute(ctx, binding.Legacy, r, body) },
	)
	switch decision.Outcome {
	case race.NewWins:
		g.completeAuthentication(w, r, decision.Target, identifier, binding.Armorial, "", true)
	case race.MigrateReuseCore, race.MigrateCreateCore:
		g.log.Info("migration required", "outcome", decision.Outcome)
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
	case race.BlockRetry:
		g.log.Info("authentication blocked", "reason", decision.Reason)
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication service temporarily unavailable.")
	case race.NoBinding:
		g.writeResult(w, decision.Target)
	default:
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication service temporarily unavailable.")
	}
}

func (g *Gateway) completeAuthentication(w http.ResponseWriter, r *http.Request, result race.Result, identifier string, winner binding.Winner, migrationID string, createBinding bool) {
	if result.Err != nil || result.StatusCode != http.StatusOK {
		g.writeResult(w, result)
		return
	}
	response, err := protocol.DecodeAuthenticateResponse(result.Body)
	if err != nil {
		writeProtocolError(w, http.StatusBadGateway, "ServiceUnavailableException", "Upstream returned an invalid response.")
		return
	}
	if createBinding {
		if _, err := g.store.BindIfAbsent(r.Context(), identifier, winner, migrationID); err != nil {
			g.log.Error("permanent binding failed", "reason", "conflict_or_storage_error")
			writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication could not be completed.")
			return
		}
	}
	if err := g.store.BindToken(r.Context(), response.AccessToken, identifier, winner); err != nil {
		g.log.Error("token binding failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication could not be completed.")
		return
	}
	g.writeResult(w, result)
}

func (g *Gateway) refresh(w http.ResponseWriter, r *http.Request) {
	body, ok := g.readRequestBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeTokenRequest(body)
	if err != nil {
		protocol.WriteInvalidToken(w)
		return
	}
	bound, err := g.bindingByToken(w, r, request.AccessToken)
	if err != nil {
		return
	}
	result := g.execute(r.Context(), bound.Winner, r, body)
	if result.Err != nil || result.StatusCode != http.StatusOK {
		g.writeResult(w, result)
		return
	}
	response, err := protocol.DecodeAuthenticateResponse(result.Body)
	if err != nil {
		writeProtocolError(w, http.StatusBadGateway, "ServiceUnavailableException", "Upstream returned an invalid response.")
		return
	}
	if err := g.store.BindTokenByHash(r.Context(), response.AccessToken, bound.IdentifierHash, bound.Winner); err != nil {
		g.log.Error("refreshed token binding failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication could not be completed.")
		return
	}
	g.writeResult(w, result)
}

func (g *Gateway) validate(w http.ResponseWriter, r *http.Request) {
	g.tokenOperation(w, r)
}

func (g *Gateway) invalidate(w http.ResponseWriter, r *http.Request) {
	g.tokenOperation(w, r)
}

func (g *Gateway) tokenOperation(w http.ResponseWriter, r *http.Request) {
	body, ok := g.readRequestBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeTokenRequest(body)
	if err != nil {
		protocol.WriteInvalidToken(w)
		return
	}
	bound, err := g.bindingByToken(w, r, request.AccessToken)
	if err != nil {
		return
	}
	g.writeResult(w, g.execute(r.Context(), bound.Winner, r, body))
}

func (g *Gateway) signout(w http.ResponseWriter, r *http.Request) {
	body, ok := g.readRequestBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeSignoutRequest(body)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "IllegalArgumentException", "Bad request.")
		return
	}
	identifier, err := protocol.NormalizeIdentifier(request.Username)
	if err != nil {
		writeProtocolError(w, http.StatusBadRequest, "IllegalArgumentException", "Bad request.")
		return
	}
	winner := binding.Legacy
	bound, err := g.store.GetBinding(r.Context(), identifier)
	if err == nil {
		winner = bound.Winner
	} else if !errors.Is(err, binding.ErrNotFound) {
		g.log.Error("signout binding lookup failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Service temporarily unavailable.")
		return
	}
	g.writeResult(w, g.execute(r.Context(), winner, r, body))
}

func (g *Gateway) bindingByToken(w http.ResponseWriter, r *http.Request, accessToken string) (binding.Binding, error) {
	bound, err := g.store.GetBindingByToken(r.Context(), accessToken)
	if errors.Is(err, binding.ErrNotFound) {
		protocol.WriteInvalidToken(w)
		return binding.Binding{}, err
	}
	if err != nil {
		g.log.Error("token binding lookup failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Service temporarily unavailable.")
		return binding.Binding{}, err
	}
	return bound, nil
}

func (g *Gateway) readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, g.maxRequestBody+1))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProtocolError(w, http.StatusRequestEntityTooLarge, "IllegalArgumentException", "Request body too large.")
		} else if r.Context().Err() == nil {
			writeProtocolError(w, http.StatusBadRequest, "IllegalArgumentException", "Bad request.")
		}
		return nil, false
	}
	if int64(len(body)) > g.maxRequestBody {
		writeProtocolError(w, http.StatusRequestEntityTooLarge, "IllegalArgumentException", "Request body too large.")
		return nil, false
	}
	return body, true
}
