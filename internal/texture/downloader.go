package texture

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var (
	ErrInvalidURL         = errors.New("invalid texture URL")
	ErrBlockedAddress     = errors.New("texture URL resolves to a blocked address")
	ErrTextureTooLarge    = errors.New("texture exceeds size limit")
	ErrTextureUnavailable = errors.New("texture download unavailable")
)

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

type Options struct {
	Timeout  time.Duration
	MaxBytes int64
}

type Downloader struct {
	client      *http.Client
	maxBytes    int64
	lookupIP    func(context.Context, string) ([]net.IPAddr, error)
	dialContext func(context.Context, string, string) (net.Conn, error)
}

func New(options Options) (*Downloader, error) {
	if options.Timeout < 0 || options.MaxBytes < 0 {
		return nil, errors.New("texture download limits must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 10 * time.Second
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 4 << 20
	}
	downloader := &Downloader{
		maxBytes: options.MaxBytes,
		lookupIP: net.DefaultResolver.LookupIPAddr,
	}
	downloader.dialContext = downloader.safeDialContext
	downloader.client = &http.Client{
		Timeout: options.Timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           downloader.dial,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: options.Timeout,
			MaxIdleConns:          10,
			MaxIdleConnsPerHost:   5,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many texture redirects")
			}
			return validateTextureURL(request.URL.String())
		},
	}
	return downloader, nil
}

func (d *Downloader) Download(ctx context.Context, rawURL string) ([]byte, error) {
	if err := validateTextureURL(rawURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, ErrInvalidURL
	}
	request.Header.Set("Accept", "image/png")
	response, err := d.client.Do(request)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(err, ErrInvalidURL):
			return nil, ErrInvalidURL
		case errors.Is(err, ErrBlockedAddress):
			return nil, ErrBlockedAddress
		default:
			return nil, ErrTextureUnavailable
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrTextureUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, d.maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTextureUnavailable
	}
	if int64(len(body)) > d.maxBytes {
		return nil, ErrTextureTooLarge
	}
	return body, nil
}

func (d *Downloader) dial(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dialContext(ctx, network, address)
}

func (d *Downloader) safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrInvalidURL
	}
	addresses, err := d.lookupIP(ctx, host)
	if err != nil {
		return nil, ErrTextureUnavailable
	}
	if len(addresses) == 0 {
		return nil, ErrBlockedAddress
	}
	for _, resolved := range addresses {
		if resolved.Zone != "" || !publicIP(resolved.IP) {
			return nil, ErrBlockedAddress
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, resolved := range addresses {
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if lastErr != nil {
		return nil, ErrTextureUnavailable
	}
	return nil, ErrBlockedAddress
}

func validateTextureURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return ErrInvalidURL
	}
	if strings.TrimSpace(parsed.Host) != parsed.Host {
		return ErrInvalidURL
	}
	return nil
}

func publicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}
