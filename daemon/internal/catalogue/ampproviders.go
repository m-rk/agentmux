package catalogue

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Amp can draw on a ChatGPT subscription. `amp model-providers list`
// lists provider keys and subscriptions; this reads it only after
// checking that no key is printed in the clear, keeps nothing but a
// yes/no, and never stores or returns the output.

var (
	secretShaped = regexp.MustCompile(`^[A-Za-z0-9_\-.]{24,}$`)
	keyPrefix    = regexp.MustCompile(`(?i)^(sk|key|tok|pat|ghp|xox[a-z])[-_][A-Za-z0-9_-]{8,}`)
)

// ErrUnmasked is returned when provider output may hold a key in the
// clear. The caller must discard the output.
var ErrUnmasked = errors.New("amp model-providers output is not masked; refusing to read it")

// CheckMasked reports ErrUnmasked if any word of output looks like an
// unmasked secret: a known key prefix with a long tail, or a long run of
// letters and digits with no mask character. A masked key (stars or an
// ellipsis inside it) passes. It errs towards refusing.
func CheckMasked(output string) error {
	for _, w := range strings.Fields(output) {
		w = strings.Trim(w, `",:;()[]{}`)
		if strings.ContainsAny(w, "*•…") || strings.Contains(w, "...") {
			continue
		}
		if keyPrefix.MatchString(w) {
			return ErrUnmasked
		}
		if secretShaped.MatchString(w) && strings.ContainsAny(w, "0123456789") && strings.ContainsAny(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") && !strings.ContainsAny(w, "/") {
			return ErrUnmasked
		}
	}
	return nil
}

// ChatGPTSubscription says whether masked provider output lists a
// ChatGPT subscription. It returns the answer only.
func ChatGPTSubscription(output string) (bool, error) {
	if err := CheckMasked(output); err != nil {
		return false, err
	}
	return strings.Contains(strings.ToLower(output), "chatgpt"), nil
}

// AmpChatGPTSubscription runs `amp model-providers list` on this host and
// returns "present" or "absent", or an error. The output is checked for
// masking before it is looked at and is dropped on return.
func AmpChatGPTSubscription(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "amp", "model-providers", "list").Output()
	if err != nil {
		return "", errors.New("amp model-providers list failed")
	}
	has, err := ChatGPTSubscription(string(out))
	if err != nil {
		return "", err
	}
	if has {
		return "present", nil
	}
	return "absent", nil
}
