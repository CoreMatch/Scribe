package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/corematch/scribe/internal/binding"
	"github.com/corematch/scribe/internal/migration"
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
		if g.migrationPending != nil {
			pending, pendingErr := g.migrationPending.Pending(r.Context(), identifier)
			if pendingErr != nil || pending {
				reason := "migration_pending"
				if pendingErr != nil {
					reason = "migration_state_unknown"
				}
				g.log.Info("authentication blocked", "reason", reason)
				writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
				return
			}
		}
		g.completeAuthentication(w, r, decision.Target, identifier, binding.Armorial, "", true)
	case race.MigrateReuseCore, race.MigrateCreateCore:
		g.migrate(w, r, request, identifier, decision)
	case race.BlockRetry:
		g.log.Info("authentication blocked", "reason", decision.Reason)
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication service temporarily unavailable.")
	case race.NoBinding:
		if decision.Target.Err == nil && decision.Target.StatusCode == http.StatusOK {
			writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication could not be completed.")
			return
		}
		g.writeResult(w, decision.Target)
	default:
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Authentication service temporarily unavailable.")
	}
}

func (g *Gateway) migrate(w http.ResponseWriter, r *http.Request, request protocol.AuthenticateRequest, identifier string, decision race.Decision) {
	if g.migrator == nil {
		g.log.Info("migration blocked", "outcome", decision.Outcome, "reason", "migration_unavailable")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
		return
	}
	legacy, err := protocol.DecodeAuthenticateResponse(decision.Legacy.Body)
	if err != nil || legacy.SelectedProfile == nil {
		g.log.Info("migration blocked", "outcome", decision.Outcome, "reason", "invalid_legacy_session")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
		return
	}
	legacyUserID, legacyProperties := migrationUser(legacy.User)
	migrated, err := g.migrator.Migrate(r.Context(), migration.Input{
		Identifier:           identifier,
		Password:             request.Password,
		SelectedProfile:      *legacy.SelectedProfile,
		LegacyUserID:         legacyUserID,
		LegacyUserProperties: legacyProperties,
	})
	if err != nil {
		g.log.Info("migration blocked", "outcome", decision.Outcome, "reason", "migration_failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
		return
	}
	result, err := migratedAuthenticationResult(migrated)
	if err != nil {
		g.log.Info("migration blocked", "outcome", decision.Outcome, "reason", "invalid_migration_result")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
		return
	}
	g.completeAuthentication(w, r, result, identifier, binding.Armorial, migrated.MigrationID, true)
}

func migrationUser(user *protocol.User) (string, map[string]string) {
	if user == nil {
		return "", nil
	}
	var properties map[string]string
	for _, property := range user.Properties {
		name, ok := property["name"]
		if !ok || name == "" {
			continue
		}
		if properties == nil {
			properties = make(map[string]string)
		}
		properties[name] = property["value"]
	}
	return user.ID, properties
}

func migratedAuthenticationResult(migrated migration.Result) (race.Result, error) {
	if migrated.MigrationID == "" || migrated.Commit.MigrationID != migrated.MigrationID || migrated.Commit.Status != "committed" {
		return race.Result{}, errors.New("migration result is not committed")
	}
	session := migrated.Commit.Session
	response := protocol.AuthenticateResponse{
		AccessToken:       session.AccessToken,
		ClientToken:       session.ClientToken,
		SelectedProfile:   &session.SelectedProfile,
		AvailableProfiles: session.AvailableProfiles,
	}
	body, err := json.Marshal(response)
	if err != nil {
		return race.Result{}, err
	}
	if _, err := protocol.DecodeAuthenticateResponse(body); err != nil {
		return race.Result{}, err
	}
	return race.Result{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}, nil
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
		if errors.Is(err, binding.ErrNotFound) {
			protocol.WriteInvalidToken(w)
		}
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
	g.tokenOperation(w, r, http.StatusForbidden)
}

func (g *Gateway) invalidate(w http.ResponseWriter, r *http.Request) {
	g.tokenOperation(w, r, http.StatusNoContent)
}

func (g *Gateway) tokenOperation(w http.ResponseWriter, r *http.Request, unknownTokenStatus int) {
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
		if errors.Is(err, binding.ErrNotFound) {
			if unknownTokenStatus == http.StatusNoContent {
				w.WriteHeader(http.StatusNoContent)
			} else {
				protocol.WriteInvalidToken(w)
			}
		}
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
	} else if g.migrationPending != nil {
		pending, pendingErr := g.migrationPending.Pending(r.Context(), identifier)
		if pendingErr != nil {
			g.log.Info("signout blocked", "reason", "migration_state_unknown")
			writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Service temporarily unavailable.")
			return
		}
		if pending {
			g.log.Info("signout blocked", "reason", "migration_pending")
			writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Migration is temporarily unavailable.")
			return
		}
	}
	g.writeResult(w, g.execute(r.Context(), winner, r, body))
}

func (g *Gateway) bindingByToken(w http.ResponseWriter, r *http.Request, accessToken string) (binding.Binding, error) {
	bound, err := g.store.GetBindingByToken(r.Context(), accessToken)
	if err != nil && !errors.Is(err, binding.ErrNotFound) {
		g.log.Error("token binding lookup failed")
		writeProtocolError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "Service temporarily unavailable.")
	}
	return bound, err
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
