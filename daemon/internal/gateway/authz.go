package gateway

import (
	"path"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
)

// ValidGrants keeps the grants that are well formed: every op is one of
// gatewayapi.Ops, there is at least one op, and every session is a non-empty
// valid path.Match pattern with at least one. A grant that fails is dropped
// whole, never trimmed, so a typo can't widen access.
func ValidGrants(in []gatewayapi.Grant) []gatewayapi.Grant {
	var out []gatewayapi.Grant
	for _, g := range in {
		if validGrant(g) {
			out = append(out, g)
		}
	}
	return out
}

func validGrant(g gatewayapi.Grant) bool {
	if len(g.Ops) == 0 || len(g.Sessions) == 0 {
		return false
	}
	for _, op := range g.Ops {
		if !knownOp(op) {
			return false
		}
	}
	for _, pat := range g.Sessions {
		if pat == "" {
			return false
		}
		if _, err := path.Match(pat, ""); err != nil {
			return false
		}
	}
	return true
}

func knownOp(op string) bool {
	for _, k := range gatewayapi.Ops {
		if op == k {
			return true
		}
	}
	return false
}

func grantHasOp(g gatewayapi.Grant, op string) bool {
	for _, o := range g.Ops {
		if o == op {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, session string) bool {
	for _, pat := range patterns {
		if ok, err := path.Match(pat, session); err == nil && ok {
			return true
		}
	}
	return false
}

// opAllowed reports whether any grant lists op at all (the check for list).
func opAllowed(grants []gatewayapi.Grant, op string) bool {
	for _, g := range grants {
		if grantHasOp(g, op) {
			return true
		}
	}
	return false
}

// sessionAllowed reports whether any grant lists op and has a session pattern
// matching session ("<instance>@<host>", no #thread).
func sessionAllowed(grants []gatewayapi.Grant, op, session string) bool {
	for _, g := range grants {
		if grantHasOp(g, op) && matchAny(g.Sessions, session) {
			return true
		}
	}
	return false
}
