package provision

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// claudeCredentialsFile is Claude Code's plain-file OAuth credential store on
// Linux (macOS instead uses Keychain — see tokenexpiry_darwin.go). Its exact
// structure isn't documented, so checkClaudeTokenExpiry walks the parsed JSON
// looking for "expiresAt"/"refreshTokenExpiresAt" keys at any nesting level
// rather than assuming a fixed shape, and reads only those two timestamps —
// never the accessToken/refreshToken values themselves.
const claudeCredentialsFile = ".claude/.credentials.json"

func checkClaudeTokenExpiry(runUser string) (TokenExpiryStatus, error) {
	home, err := resumeHomeDir(runUser)
	if err != nil {
		return TokenExpiryStatus{}, err
	}
	path := filepath.Join(home, claudeCredentialsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Not logged in yet, or logged in via a non-OAuth method (e.g.
			// ANTHROPIC_API_KEY) that has no expiry to track.
			return TokenExpiryStatus{}, nil
		}
		return TokenExpiryStatus{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return TokenExpiryStatus{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	var status TokenExpiryStatus
	var foundAccess, foundRefresh bool
	walkExpiryKeys(parsed, &status.AccessExpiresAt, &foundAccess, &status.RefreshExpiresAt, &foundRefresh)
	status.Supported = foundAccess || foundRefresh
	return status, nil
}

// walkExpiryKeys recursively scans v (the result of unmarshaling arbitrary
// JSON into `any`) for "expiresAt" and "refreshTokenExpiresAt" keys holding
// numeric (epoch) values, setting *access/*refresh and their found flags the
// first time each is seen.
func walkExpiryKeys(v any, access *time.Time, foundAccess *bool, refresh *time.Time, foundRefresh *bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if num, ok := val.(float64); ok {
				switch k {
				case "expiresAt":
					*access = epochToTime(num)
					*foundAccess = true
				case "refreshTokenExpiresAt":
					*refresh = epochToTime(num)
					*foundRefresh = true
				}
			}
			walkExpiryKeys(val, access, foundAccess, refresh, foundRefresh)
		}
	case []any:
		for _, item := range t {
			walkExpiryKeys(item, access, foundAccess, refresh, foundRefresh)
		}
	}
}

// epochToTime guesses seconds vs. milliseconds the same way most JS-authored
// tools' epoch fields need to be guessed: anything too large to be a
// plausible seconds-since-1970 value (roughly year 33658) is treated as
// milliseconds instead.
func epochToTime(v float64) time.Time {
	if v > 1e12 {
		return time.UnixMilli(int64(v))
	}
	return time.Unix(int64(v), 0)
}
