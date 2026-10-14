package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type requestPolicy struct {
	issuerHost   string
	upstreamHost string
	origins      map[string]bool
	scheme       string
}

func newPolicy(opts Options, issuer *url.URL) (*requestPolicy, error) {
	p := &requestPolicy{
		issuerHost: canonicalHost(issuer.Host, issuer.Scheme),
		origins:    map[string]bool{canonicalOrigin(issuer): true},
		scheme:     issuer.Scheme,
	}
	// A reverse proxy may forward either the external Host or its explicit
	// loopback upstream address. Wildcard binds never imply arbitrary hosts,
	// and no other localhost alias/port is automatically trusted.
	listenHost, _, _ := net.SplitHostPort(opts.Listen)
	if isLoopback(listenHost) {
		p.upstreamHost = canonicalHost(opts.Listen, "http")
	}
	for _, raw := range opts.AllowedOrigins {
		origin, err := url.Parse(raw)
		if err != nil || !validOriginURL(origin) {
			return nil, errors.New("each HTTP allowed origin must be an HTTPS or loopback HTTP origin without a path")
		}
		p.origins[canonicalOrigin(origin)] = true
	}
	return p, nil
}

func (p *requestPolicy) wrap(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		// Forwarded and X-Forwarded-* headers are intentionally ignored. The
		// explicit public issuer and private listen address are the only trust
		// roots for routing; nginx must preserve one of these Host values.
		issuerHost := canonicalHost(r.Host, p.scheme)
		upstreamHost := canonicalHost(r.Host, "http")
		if issuerHost != p.issuerHost && (p.upstreamHost == "" || upstreamHost != p.upstreamHost) {
			http.Error(w, "Host is not allowed", http.StatusForbidden)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 {
			http.Error(w, "Origin is not allowed", http.StatusForbidden)
			return
		}
		if len(origins) == 1 {
			origin, err := url.Parse(origins[0])
			if err != nil || !validOriginURL(origin) {
				http.Error(w, "Origin is not allowed", http.StatusForbidden)
				return
			}
			if publicCORSRoute(r.URL.Path) {
				// These routes are stateless discovery or protocol exchanges.
				// They do not accept authorization-session cookies as authority.
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if p.origins[canonicalOrigin(origin)] {
				w.Header().Set("Access-Control-Allow-Origin", origins[0])
				w.Header().Add("Vary", "Origin")
			} else {
				http.Error(w, "Origin is not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate, MCP-Session-Id, Content-Disposition, Content-Length, Content-Range, Accept-Ranges, ETag, X-Content-SHA256")
		}
		if r.Method == http.MethodOptions {
			p.preflight(w, r, len(origins) == 1)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (p *requestPolicy) preflight(w http.ResponseWriter, r *http.Request, hasOrigin bool) {
	if !hasOrigin {
		http.Error(w, "CORS preflight requires an Origin", http.StatusBadRequest)
		return
	}
	methods := routeMethods(r.URL.Path)
	requested := r.Header.Get("Access-Control-Request-Method")
	allowed := false
	for _, method := range methods {
		if method == requested {
			allowed = true
			break
		}
	}
	if !allowed {
		if len(methods) == 0 {
			http.NotFound(w, r)
		} else {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			http.Error(w, "Method is not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(methods, ", "))
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version, MCP-Session-Id, Mcp-Method, Mcp-Name, Last-Event-ID, Range, If-Range, If-None-Match, If-Modified-Since")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.WriteHeader(http.StatusNoContent)
}

func routeMethods(path string) []string {
	switch {
	case path == "/mcp":
		return []string{http.MethodPost, http.MethodGet, http.MethodDelete}
	case strings.HasPrefix(path, "/transfers/uploads/"):
		return []string{http.MethodPut}
	case strings.HasPrefix(path, "/transfers/downloads/"):
		return []string{http.MethodGet, http.MethodHead}
	case path == "/oauth/token" || path == "/oauth/register" || path == "/oauth/revoke":
		return []string{http.MethodPost}
	case path == "/oauth/authorize":
		return []string{http.MethodGet, http.MethodPost}
	case path == "/" || path == "/healthz" || strings.HasPrefix(path, "/.well-known/"):
		return []string{http.MethodGet, http.MethodHead}
	default:
		return nil
	}
}

func publicCORSRoute(path string) bool {
	return path == "/oauth/token" || path == "/oauth/register" || path == "/oauth/revoke" ||
		strings.HasPrefix(path, "/.well-known/")
}

func securityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

func validOriginURL(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname()))
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalOrigin(u *url.URL) string { return u.Scheme + "://" + canonicalHost(u.Host, u.Scheme) }

// canonicalHost permits the standard optional port and case normalization but
// never treats malformed Host input as an alias for a trusted hostname.
func canonicalHost(raw, scheme string) string {
	if strings.ContainsAny(raw, "/\\, \t\r\n") {
		return ""
	}
	u, err := url.Parse("//" + raw)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if host == "" {
		return ""
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
