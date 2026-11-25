package texture

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDownloadRetrievesTextureOverTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "image/png" {
			t.Fatalf("Accept = %q", r.Header.Get("Accept"))
		}
		_, _ = io.WriteString(w, "texture")
	}))
	defer server.Close()
	downloader := localTestDownloader(t, server, 1024)

	body, err := downloader.Download(context.Background(), server.URL+"/skin.png")
	if err != nil || string(body) != "texture" {
		t.Fatalf("Download() = %q, %v", body, err)
	}
}

func TestDownloadEnforcesResponseLimitsAndStatus(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		max     int64
		want    error
	}{
		{
			name: "too large",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "texture")
			},
			max:  3,
			want: ErrTextureTooLarge,
		},
		{
			name: "not found",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			max:  1024,
			want: ErrTextureUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tt.handler)
			defer server.Close()
			downloader := localTestDownloader(t, server, tt.max)
			if _, err := downloader.Download(context.Background(), server.URL); !errors.Is(err, tt.want) {
				t.Fatalf("Download() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDownloadRejectsUnsafeURLsAndRedirects(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "http://example.com/texture")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "texture")
	}))
	defer server.Close()
	downloader := localTestDownloader(t, server, 1024)
	for _, rawURL := range []string{
		"http://example.com/texture",
		"https://user:pass@example.com/texture",
		"https://example.com/texture#fragment",
		"/texture",
		"https:///texture",
	} {
		if _, err := downloader.Download(context.Background(), rawURL); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("Download(%q) error = %v, want invalid URL", rawURL, err)
		}
	}
	if _, err := downloader.Download(context.Background(), server.URL+"/redirect"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("redirect error = %v, want invalid URL", err)
	}
}

func TestDownloadTimesOutAndSanitizesErrors(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	downloader := localTestDownloader(t, server, 1024)
	downloader.client.Timeout = 20 * time.Millisecond
	if _, err := downloader.Download(context.Background(), server.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	failed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failedURL := failed.URL + "/texture?token=secret"
	failed.Close()
	downloader = localTestDownloader(t, server, 1024)
	_, err := downloader.Download(context.Background(), failedURL)
	if !errors.Is(err, ErrTextureUnavailable) {
		t.Fatalf("failure error = %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), failed.URL) {
		t.Fatalf("failure error leaked texture URL: %v", err)
	}
}

func TestSafeDialRejectsRebindingAndMixedDNSResults(t *testing.T) {
	private := net.ParseIP("127.0.0.1")
	public := net.ParseIP("93.184.216.34")
	tests := []struct {
		name      string
		addresses []net.IPAddr
	}{
		{name: "private", addresses: []net.IPAddr{{IP: private}}},
		{name: "mixed", addresses: []net.IPAddr{{IP: public}, {IP: private}}},
		{name: "zone", addresses: []net.IPAddr{{IP: net.ParseIP("fe80::1"), Zone: "eth0"}}},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downloader, err := New(Options{})
			if err != nil {
				t.Fatal(err)
			}
			downloader.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
				return tt.addresses, nil
			}
			if connection, err := downloader.safeDialContext(context.Background(), "tcp", "texture.example.com:443"); connection != nil || !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("safeDialContext() = %v, %v", connection, err)
			}
		})
	}
}

func TestPublicIPBlocksSpecialNetworks(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.0.2.1", "192.168.1.1", "198.18.0.1", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "240.0.0.1", "::1", "::", "64:ff9b::7f00:1",
		"2001:db8::1", "fc00::1", "fe80::1", "fec0::1", "ff02::1",
	}
	for _, raw := range blocked {
		if publicIP(net.ParseIP(raw)) {
			t.Errorf("publicIP(%q) unexpectedly allowed special address", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(raw)) {
			t.Errorf("publicIP(%q) unexpectedly blocked public address", raw)
		}
	}
}

func TestDownloaderRejectsInvalidOptions(t *testing.T) {
	for _, options := range []Options{{Timeout: -1}, {MaxBytes: -1}} {
		if _, err := New(options); err == nil {
			t.Fatalf("New(%+v) unexpectedly succeeded", options)
		}
	}
}

func localTestDownloader(t *testing.T, server *httptest.Server, maxBytes int64) *Downloader {
	t.Helper()
	downloader, err := New(Options{Timeout: time.Second, MaxBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	downloader.dialContext = (&net.Dialer{}).DialContext
	transport := downloader.client.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return downloader
}
