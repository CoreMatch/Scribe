package protocol

import (
	"strings"
	"testing"
)

func TestDecodeAuthenticateRequest(t *testing.T) {
	request, err := DecodeAuthenticateRequest([]byte(`{"username":"Player","password":"secret","clientToken":"client","requestUser":true}`))
	if err != nil {
		t.Fatalf("DecodeAuthenticateRequest() error = %v", err)
	}
	if request.Username != "Player" || request.Password != "secret" || request.ClientToken != "client" || request.RequestUser == nil || !*request.RequestUser {
		t.Fatalf("DecodeAuthenticateRequest() = %+v", request)
	}
}

func TestDecodeAuthenticateRequestRejectsMissingCredentials(t *testing.T) {
	for _, body := range []string{`{}`, `{"username":"Player"}`, `{"password":"secret"}`, `not-json`} {
		if _, err := DecodeAuthenticateRequest([]byte(body)); err == nil {
			t.Fatalf("DecodeAuthenticateRequest(%q) unexpectedly succeeded", body)
		}
	}
}

func TestDecodeAuthenticateResponse(t *testing.T) {
	response, err := DecodeAuthenticateResponse([]byte(`{"accessToken":"access","clientToken":"client","selectedProfile":{"id":"12345678123412341234567812345678","name":"Player"},"availableProfiles":[{"id":"87654321876543218765432187654321","name":"Other"}]}`))
	if err != nil {
		t.Fatalf("DecodeAuthenticateResponse() error = %v", err)
	}
	if response.SelectedProfile == nil || response.SelectedProfile.ID != "12345678123412341234567812345678" || len(response.AvailableProfiles) != 1 {
		t.Fatalf("DecodeAuthenticateResponse() = %+v", response)
	}
}

func TestDecodeAuthenticateResponseRejectsMalformedSuccess(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"accessToken":"access"}`,
		`{"accessToken":"access","clientToken":"client","selectedProfile":{"id":"12345678123412341234567812345678"}}`,
		`{"accessToken":"access","clientToken":"client","selectedProfile":{"id":"1234567812341234-1234-1234-1234-123456789012","name":"Player"}}`,
		`{"accessToken":"access","clientToken":"client","availableProfiles":[{"id":"not-a-uuid","name":"Player"}]}`,
		`{"accessToken":"access","clientToken":"client","availableProfiles":[{"id":"12345678123412341234567812345678","name":" "}]}`,
		`not-json`,
	} {
		if _, err := DecodeAuthenticateResponse([]byte(body)); err == nil {
			t.Fatalf("DecodeAuthenticateResponse(%q) unexpectedly succeeded", body)
		}
	}
}

func TestDecodeTokenRequest(t *testing.T) {
	request, err := DecodeTokenRequest([]byte(`{"accessToken":"access","clientToken":"client"}`))
	if err != nil || request.AccessToken != "access" || request.ClientToken != "client" {
		t.Fatalf("DecodeTokenRequest() = %+v, %v", request, err)
	}
	for _, body := range []string{`{}`, `{"clientToken":"client"}`, `not-json`} {
		if _, err := DecodeTokenRequest([]byte(body)); err == nil {
			t.Fatalf("DecodeTokenRequest(%q) unexpectedly succeeded", body)
		}
	}
}

func TestDecodeSignoutRequest(t *testing.T) {
	request, err := DecodeSignoutRequest([]byte(`{"username":"Player","password":"secret"}`))
	if err != nil || request.Username != "Player" || request.Password != "secret" {
		t.Fatalf("DecodeSignoutRequest() = %+v, %v", request, err)
	}
	for _, body := range []string{`{}`, `{"username":"Player"}`, `not-json`} {
		if _, err := DecodeSignoutRequest([]byte(body)); err == nil {
			t.Fatalf("DecodeSignoutRequest(%q) unexpectedly succeeded", body)
		}
	}
}

func TestNormalizeIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: " Player ", want: "Player"},
		{input: "Player@Example.COM", want: "Player@example.com"},
		{input: "e\u0301@example.com", want: "é@example.com"},
	}
	for _, tt := range tests {
		got, err := NormalizeIdentifier(tt.input)
		if err != nil || got != tt.want {
			t.Fatalf("NormalizeIdentifier(%q) = %q, %v; want %q", tt.input, got, err, tt.want)
		}
	}
}

func TestNormalizeIdentifierRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"", "   ", "player\nname", strings.Repeat("a", 255)} {
		if _, err := NormalizeIdentifier(input); err == nil {
			t.Fatalf("NormalizeIdentifier(%q) unexpectedly succeeded", input)
		}
	}
}
