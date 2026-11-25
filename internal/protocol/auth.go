package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type AuthenticateRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	ClientToken string `json:"clientToken,omitempty"`
	RequestUser *bool  `json:"requestUser,omitempty"`
}

type AuthenticateResponse struct {
	AccessToken       string    `json:"accessToken"`
	ClientToken       string    `json:"clientToken"`
	SelectedProfile   *Profile  `json:"selectedProfile"`
	AvailableProfiles []Profile `json:"availableProfiles,omitempty"`
	User              *User     `json:"user,omitempty"`
}

type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type User struct {
	ID         string              `json:"id"`
	Properties []map[string]string `json:"properties,omitempty"`
}

type ErrorResponse struct {
	Error        string `json:"error"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Cause        string `json:"cause,omitempty"`
}

func DecodeAuthenticateRequest(data []byte) (AuthenticateRequest, error) {
	var request AuthenticateRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return AuthenticateRequest{}, fmt.Errorf("decode authenticate request: %w", err)
	}
	if strings.TrimSpace(request.Username) == "" || request.Password == "" {
		return AuthenticateRequest{}, errors.New("username and password are required")
	}
	if !utf8.ValidString(request.Username) || !utf8.ValidString(request.Password) {
		return AuthenticateRequest{}, errors.New("credentials must be valid UTF-8")
	}
	return request, nil
}

func DecodeAuthenticateResponse(data []byte) (AuthenticateResponse, error) {
	var response AuthenticateResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return AuthenticateResponse{}, fmt.Errorf("decode authenticate response: %w", err)
	}
	if response.AccessToken == "" || response.ClientToken == "" {
		return AuthenticateResponse{}, errors.New("authenticate response is missing tokens")
	}
	if response.SelectedProfile != nil {
		if response.SelectedProfile.ID == "" || response.SelectedProfile.Name == "" {
			return AuthenticateResponse{}, errors.New("selectedProfile is incomplete")
		}
	}
	return response, nil
}

func NormalizeIdentifier(identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" || len(identifier) > 254 || !utf8.ValidString(identifier) {
		return "", errors.New("identifier must contain between 1 and 254 UTF-8 characters")
	}
	for _, r := range identifier {
		if unicode.IsControl(r) {
			return "", errors.New("identifier must not contain control characters")
		}
	}
	identifier = norm.NFC.String(identifier)
	address, err := mail.ParseAddress(identifier)
	if err == nil && address.Address == identifier {
		at := strings.LastIndexByte(identifier, '@')
		return identifier[:at+1] + strings.ToLower(identifier[at+1:]), nil
	}
	// Legacy username case semantics are unspecified, so case must be preserved.
	return identifier, nil
}
