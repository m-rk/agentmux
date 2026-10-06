package gateway

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// selfUpdateOp serves the AMUX-29 pull-updater ops: the hub publishes the
// shipped commit per repo (ship-publish), and reads back the installed
// commit per repo (versions) plus the updater's own log (selfupdate-log)
// to confirm the Mac is on a commit or warn when it lags. All three are
// host-wide, like gc: no session target, so the check is opAllowed, not
// sessionAllowed. Bodies stay small and replies carry no secrets, only
// commit shas and the updater's own log lines.
//
// Publish is a merge, not a replace: repos absent from the request keep
// their gate, so a publish for agentmux never clears mergentic's. Only
// known repos with well-formed shas reach the gate; anything else is
// ignored and reported as such (the reply echoes the gate, not the
// request, so the caller sees what actually landed).
func (s *Server) selfUpdateOp(w http.ResponseWriter, r *http.Request, a *access, id Identity, op string, body io.Reader) {
	if !s.other.allow(a.principal) {
		s.rateLimited(w, a, s.other.limit)
		return
	}
	a.target = "-"
	if !opAllowed(id.Grants, op) {
		s.refuse(w, a, http.StatusForbidden, safesend.ReasonForbidden, op+" is not permitted")
		return
	}
	switch op {
	case gatewayapi.OpShipPublish:
		var req gatewayapi.ShipPublishRequest
		if !s.decode(w, a, body, &req, true) {
			return
		}
		if req.Commits == nil {
			req.Commits = map[string]string{}
		}
		out, err := s.backend.ShipPublish(r.Context(), req.Commits)
		if err != nil {
			e := ops.AsError(err)
			s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
			return
		}
		s.reply(w, a, http.StatusOK, gatewayapi.ShipPublishResponse{Shipped: out})
	case gatewayapi.OpVersions:
		out, err := s.backend.Versions(r.Context())
		if err != nil {
			e := ops.AsError(err)
			s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
			return
		}
		s.reply(w, a, http.StatusOK, gatewayapi.VersionsResponse{Host: s.backend.Host(), Versions: out})
	case gatewayapi.OpSelfUpdateLog:
		var req gatewayapi.SelfUpdateLogRequest
		if !s.decode(w, a, body, &req, true) {
			return
		}
		lines, err := s.backend.SelfUpdateLog(r.Context(), req.Lines)
		if err != nil {
			e := ops.AsError(err)
			s.refuse(w, a, gatewayapi.HTTPStatus(e.Reason), e.Reason, e.Detail)
			return
		}
		s.reply(w, a, http.StatusOK, gatewayapi.SelfUpdateLogResponse{Lines: lines})
	}
}

// SelfUpdateFiles locates the updater state for this host: the gate, the
// versions record and the log all live under the daemon user's home.
// Exported so LocalBackend and tests share the one layout.
func SelfUpdateFiles(home string) (shipped, versions, log string) {
	return selfupdate.ShippedPath(home), selfupdate.VersionsPath(home), selfupdate.LogPath(home)
}

// selfUpdateHome is the daemon user's home: the updater runs as the same
// user (a per-user LaunchAgent on macOS), so its state is theirs.
func selfUpdateHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// ShipPublish merges commits into the ship gate file and returns the
// gate. Unknown repos and malformed shas are dropped, never installed.
func ShipPublish(_ context.Context, home string, commits map[string]string) (map[string]string, error) {
	if home == "" {
		return nil, ops.Refuse(safesend.ReasonFailed, "cannot locate the updater state dir")
	}
	shipped, _, _ := SelfUpdateFiles(home)
	gate, err := selfupdate.LoadCommits(shipped)
	if err != nil {
		return nil, ops.Refuse(safesend.ReasonFailed, "reading the ship gate: %v", err)
	}
	for repo, sha := range commits {
		if !selfupdate.KnownRepo(repo) || !selfupdate.ValidSHA(sha) {
			continue
		}
		gate[repo] = sha
	}
	if err := selfupdate.SaveCommits(shipped, gate); err != nil {
		return nil, ops.Refuse(safesend.ReasonFailed, "writing the ship gate: %v", err)
	}
	return gate, nil
}

// InstalledVersions reads the installed-commit record. A missing file is
// not an error: nothing is installed yet.
func InstalledVersions(_ context.Context, home string) (map[string]string, error) {
	if home == "" {
		return nil, ops.Refuse(safesend.ReasonFailed, "cannot locate the updater state dir")
	}
	_, versions, _ := SelfUpdateFiles(home)
	got, err := selfupdate.LoadCommits(versions)
	if err != nil {
		return nil, ops.Refuse(safesend.ReasonFailed, "reading installed versions: %v", err)
	}
	return got, nil
}

// TailLog returns the last n lines of the updater event log. A missing
// log is not an error: the updater has never run on this host.
func TailLog(home string, n int) ([]string, error) {
	if home == "" {
		return nil, ops.Refuse(safesend.ReasonFailed, "cannot locate the updater state dir")
	}
	_, _, logPath := SelfUpdateFiles(home)
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, ops.Refuse(safesend.ReasonFailed, "reading the updater log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
