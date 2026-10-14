package oauth

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// lookupClient accepts both dynamically registered opaque IDs and HTTPS client
// metadata document IDs. Metadata is public and self-asserted; it grants no
// backend authority. The user still explicitly authorizes their own key, and
// every redirect must exactly match the validated client document.
func (s *Server) lookupClient(ctx context.Context, id string) (client, error) {
	if id == "" || len(id) > 2048 {
		return client{}, errors.New("invalid client ID")
	}
	c, expires, err := s.registeredClient(ctx, id)
	if err == nil && (expires == 0 || expires > time.Now().Unix()) {
		return c, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return client{}, err
	}
	if !strings.HasPrefix(id, "https://") {
		return client{}, errors.New("unknown client")
	}
	c, err = fetchClientMetadata(ctx, id)
	if err != nil {
		return client{}, err
	}
	if err = s.validateMetadataClient(&c); err != nil {
		return client{}, err
	}
	redirects, _ := json.Marshal(c.Redirects)
	grants, _ := json.Marshal(c.Grants)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return client{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients`).Scan(&count); err != nil {
		return client{}, err
	}
	var existing int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients WHERE id=?`, id).Scan(&existing); err != nil {
		return client{}, err
	}
	if existing == 0 && count >= s.opts.MaxClients {
		return client{}, errors.New("client registration capacity reached")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO clients(id,name,redirects,grant_types,created_at,metadata_expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,redirects=excluded.redirects,grant_types=excluded.grant_types,metadata_expires_at=excluded.metadata_expires_at`, id, c.Name, string(redirects), string(grants), time.Now().Unix(), time.Now().Add(5*time.Minute).Unix())
	if err != nil {
		return client{}, err
	}
	if err = tx.Commit(); err != nil {
		return client{}, err
	}
	return c, nil
}

// validateMetadataClient intersects a reusable client's advertised grant types
// with this server's capabilities. Claude's hosted document also advertises
// jwt-bearer for other authorization servers; that does not make its ordinary
// code/PKCE flow invalid or enable that grant here. Dynamic registration remains
// strict because it registers a client specifically with this server.
func (s *Server) validateMetadataClient(c *client) error {
	if len(c.Grants) > 0 {
		grants := make([]string, 0, len(c.Grants))
		for _, grant := range c.Grants {
			if grant == "authorization_code" || grant == "refresh_token" {
				grants = append(grants, grant)
			}
		}
		if len(grants) == 0 {
			return errors.New("client metadata must support authorization_code")
		}
		c.Grants = grants
	}
	return s.validateClient(c)
}

// fetchClientMetadata isolates outbound discovery from the backend HTTP client.
// It resolves once, rejects every non-public result, and pins that resolution
// into DialContext. Redirects, proxies, credentials and connection reuse are
// disabled, preventing DNS rebinding and discovery requests into local Vela or
// deployment networks. TLS still verifies the original document hostname.
func fetchClientMetadata(ctx context.Context, id string) (client, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" || u.User != nil || u.Fragment != "" || len(id) > 2048 {
		return client{}, errors.New("invalid client metadata URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return client{}, errors.New("client metadata host cannot be resolved")
	}
	for _, ip := range addresses {
		if !publicAddress(ip) {
			return client{}, errors.New("client metadata URL must resolve only to public IP addresses")
		}
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, MaxResponseHeaderBytes: 16 * 1024}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialHost, dialPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(dialHost, host) || dialPort != port {
			return nil, errors.New("unexpected discovery destination")
		}
		dialer := net.Dialer{Timeout: 3 * time.Second}
		var last error
		for _, ip := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}
	defer transport.CloseIdleConnections()
	clientHTTP := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return client{}, err
	}
	req.Header.Set("Accept", "application/json")
	response, err := clientHTTP.Do(req)
	if err != nil {
		return client{}, errors.New("client metadata could not be fetched")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return client{}, errors.New("client metadata must return HTTP 200 without redirects")
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !(strings.HasPrefix(contentType, "application/") && strings.HasSuffix(contentType, "+json"))) {
		return client{}, errors.New("client metadata must be JSON")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32*1024+1))
	if err != nil || len(data) > 32*1024 {
		return client{}, errors.New("client metadata exceeds the size limit")
	}
	var c client
	if err = json.Unmarshal(data, &c); err != nil || c.ID != id || c.Name == "" || len(c.Redirects) == 0 {
		return client{}, errors.New("client metadata client_id must exactly match its URL")
	}
	return c, nil
}

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
