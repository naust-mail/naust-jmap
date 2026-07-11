package webpush

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// ErrPrivateHost means the subscription URL resolved to an address on
// the server's own network; RFC 8620 section 8.6 requires the URL to
// be externally resolvable to prevent server-side request forgery.
var ErrPrivateHost = errors.New("webpush: subscription URL resolves to a private or local address")

// Sender POSTs push messages to subscription URLs. The zero value is
// ready to use and safe: HTTPS only, redirects refused, and
// connections to private, loopback, link-local, and unspecified
// addresses blocked at dial time (after DNS resolution, so a rebinding
// name cannot dodge the check).
type Sender struct {
	// Client overrides the default HTTP client. A custom client keeps
	// the HTTPS-only rule but does NOT get the private address blocking
	// or redirect refusal unless it provides them itself; supply one
	// only when you take on that responsibility (tests do).
	Client *http.Client
}

// Send POSTs one push message per RFC 8620 section 7.2: content type
// application/json, a TTL header (RFC 8030 section 5.2), and - when
// the subscription has keys - the body encrypted per RFC 8291 under
// content encoding aes128gcm. It returns the HTTP status code; a 429
// obliges the caller to reduce its push frequency.
func (s *Sender) Send(ctx context.Context, rawURL string, keys *jmap.PushKeys, payload []byte, ttlSeconds int) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	// The subscription URL MUST begin with "https://" (section 7.2).
	if u.Scheme != "https" {
		return 0, fmt.Errorf("webpush: subscription URL %q is not https", rawURL)
	}

	body := payload
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("TTL", strconv.Itoa(ttlSeconds))
	if keys != nil {
		uaPublic, authSecret, err := DecodeKeys(keys.P256dh, keys.Auth)
		if err != nil {
			return 0, err
		}
		if body, err = Encrypt(uaPublic, authSecret, payload); err != nil {
			return 0, err
		}
		headers.Set("Content-Encoding", "aes128gcm")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = headers
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (s *Sender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient()
}

var defaultClient = sync.OnceValue(func() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
				Control: refusePrivate,
			}).DialContext,
			MaxIdleConns:    16,
			IdleConnTimeout: time.Minute,
		},
		// A push endpoint has no business redirecting, and following
		// one could bounce the request somewhere the original URL
		// check never saw.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("webpush: push endpoints must not redirect")
		},
	}
})

// refusePrivate rejects the connection if the resolved address is not
// externally routable (RFC 8620 section 8.6). Running as a dial
// control means every connection attempt is checked with the literal
// IP being connected to, so DNS tricks cannot slip past it.
func refusePrivate(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	addr := ap.Addr().Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return ErrPrivateHost
	}
	return nil
}
