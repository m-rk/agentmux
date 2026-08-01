package provision

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWalkExpiryKeysNestedShape(t *testing.T) {
	// Mirrors the shape a real .credentials.json is expected to have:
	// timestamps nested under a provider-specific object, not top-level —
	// walkExpiryKeys shouldn't assume a fixed structure.
	body := []byte(`{
		"claudeAiOauth": {
			"accessToken": "sk-not-a-real-token",
			"refreshToken": "sk-not-a-real-refresh-token",
			"expiresAt": 0,
			"refreshTokenExpiresAt": 1785456449000,
			"scopes": ["user:inference"]
		}
	}`)

	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}

	var access, refresh time.Time
	var foundAccess, foundRefresh bool
	walkExpiryKeys(parsed, &access, &foundAccess, &refresh, &foundRefresh)

	if !foundAccess || !access.Equal(time.Unix(0, 0)) {
		t.Errorf("access = %v (found=%v), want epoch 0", access, foundAccess)
	}
	if !foundRefresh {
		t.Fatal("refreshTokenExpiresAt not found")
	}
	wantRefresh := time.UnixMilli(1785456449000)
	if !refresh.Equal(wantRefresh) {
		t.Errorf("refresh = %v, want %v", refresh, wantRefresh)
	}
}

func TestWalkExpiryKeysMissing(t *testing.T) {
	var parsed any
	if err := json.Unmarshal([]byte(`{"loggedIn": true}`), &parsed); err != nil {
		t.Fatal(err)
	}
	var access, refresh time.Time
	var foundAccess, foundRefresh bool
	walkExpiryKeys(parsed, &access, &foundAccess, &refresh, &foundRefresh)
	if foundAccess || foundRefresh {
		t.Errorf("expected nothing found in a shape with no expiry keys, got foundAccess=%v foundRefresh=%v", foundAccess, foundRefresh)
	}
}

func TestEpochToTime(t *testing.T) {
	cases := []struct {
		in   float64
		want time.Time
	}{
		{0, time.Unix(0, 0)},
		{1785456449, time.Unix(1785456449, 0)},         // plausible seconds value
		{1785456449000, time.UnixMilli(1785456449000)}, // plausible ms value
	}
	for _, c := range cases {
		if got := epochToTime(c.in); !got.Equal(c.want) {
			t.Errorf("epochToTime(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
