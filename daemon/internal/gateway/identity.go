package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// Identity is who a request came from, as the tailnet vouches for it.
type Identity struct {
	Node   string   // machine name (not a DNS name)
	Login  string   // owning user's login name; for logs only
	Tags   []string // ACL tags on the node; for logs only
	Grants []gatewayapi.Grant
}

// Principal is the name recorded in provenance prefixes and audit entries:
// the node name, lower-cased and cut down to what safesend.ValidToken
// accepts. Empty when the node has no usable name.
func (id Identity) Principal() string { return sanitizePrincipal(id.Node) }

func sanitizePrincipal(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', strings.ContainsRune("._:@/-", r):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	p := b.String()
	if len(p) > 64 {
		p = p[:64]
	}
	if !safesend.ValidToken(p) {
		return ""
	}
	return p
}

// Whois resolves a request's remote address ("ip:port") to an Identity.
// Any error means the caller is unknown and the request is refused.
type Whois func(ctx context.Context, remoteAddr string) (Identity, error)

// TailscaleWhois runs `tailscale whois --json` and reads the app capability
// named capability from the result.
func TailscaleWhois(tailscaleBin, capability string) Whois {
	if tailscaleBin == "" {
		tailscaleBin = "tailscale"
	}
	return func(ctx context.Context, remoteAddr string) (Identity, error) {
		// Only an ip:port goes on the command line.
		ap, err := netip.ParseAddrPort(remoteAddr)
		if err != nil {
			return Identity{}, fmt.Errorf("remote address %q: %w", remoteAddr, err)
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tailscaleBin, "whois", "--json", ap.String()).Output()
		if err != nil {
			return Identity{}, fmt.Errorf("tailscale whois %s: %w", ap.Addr(), err)
		}
		return ParseWhois(out, capability)
	}
}

// whoisJSON is the part of `tailscale whois --json` that is used. CapMap at
// the top level holds the capabilities the peer has toward this node (what a
// grant's "app" field produces); Node.CapMap is the peer's own and is not
// read. Each value is an array of the JSON objects from the grant's app
// entries; a capability with no grant is absent or null.
type whoisJSON struct {
	Node struct {
		Name         string   `json:"Name"`
		ComputedName string   `json:"ComputedName"`
		Tags         []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
	CapMap map[string]json.RawMessage `json:"CapMap"`
}

// ParseWhois reads `tailscale whois --json` output. It fails when the output
// can't be read, the node has no name, or the capability's value isn't an
// array. A missing capability is not an error: the Identity has no Grants and
// every operation is refused.
func ParseWhois(data []byte, capability string) (Identity, error) {
	var w whoisJSON
	if err := json.Unmarshal(data, &w); err != nil {
		return Identity{}, fmt.Errorf("parsing whois output: %w", err)
	}
	name := w.Node.ComputedName
	if name == "" {
		name, _, _ = strings.Cut(strings.TrimSuffix(w.Node.Name, "."), ".")
	}
	if name == "" {
		return Identity{}, errors.New("whois output has no node name")
	}
	id := Identity{Node: name, Login: w.UserProfile.LoginName, Tags: w.Node.Tags}
	raw, ok := w.CapMap[capability]
	if !ok || string(raw) == "null" {
		return id, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Identity{}, fmt.Errorf("capability %s is not an array: %w", capability, err)
	}
	var grants []gatewayapi.Grant
	for _, e := range entries {
		var g gatewayapi.Grant
		if json.Unmarshal(e, &g) != nil {
			continue // an entry we can't read grants nothing
		}
		grants = append(grants, g)
	}
	id.Grants = ValidGrants(grants)
	return id, nil
}

// cachedWhois remembers successful lookups per remote IP, so a burst of
// requests runs one `tailscale whois`. A policy change reaches the gateway
// within ttl. Failures are not cached.
type cachedWhois struct {
	next Whois
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[netip.Addr]cacheEntry
}

type cacheEntry struct {
	id      Identity
	expires time.Time
}

func newCachedWhois(next Whois, ttl time.Duration, now func() time.Time) *cachedWhois {
	return &cachedWhois{next: next, ttl: ttl, now: now, entries: map[netip.Addr]cacheEntry{}}
}

func (c *cachedWhois) lookup(ctx context.Context, remoteAddr string) (Identity, error) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return Identity{}, fmt.Errorf("remote address %q: %w", remoteAddr, err)
	}
	ip := ap.Addr().Unmap()
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[ip]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.id, nil
	}
	c.mu.Unlock()

	id, err := c.next(ctx, remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	c.mu.Lock()
	for k, e := range c.entries { // drop expired entries so the map stays small
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[ip] = cacheEntry{id: id, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return id, nil
}
