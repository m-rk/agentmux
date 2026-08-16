package provision

import "fmt"

const (
	defaultAgentmuxInstance = "agentmux"
	defaultOllamaModel      = "gpt-oss:20b-cloud"
	defaultProviderWaitSecs = "60"
)

// validateSupportedAgentProvider only guards the agent side of the
// combination now — any provider id is accepted for zero/opencode/kilo
// (an arbitrary OpenAI-compatible gateway, not just the built-in ollama
// backend). Provider-specific requirements (e.g. a non-ollama provider
// needing an explicit base URL) are enforced separately by
// resolveBaseURL, so the error message there can name the missing flag
// instead of a generic "unsupported combination".
func validateSupportedAgentProvider(agent, provider string) error {
	switch agent {
	case "zero", "opencode", "kilo":
		return nil
	default:
		return fmt.Errorf("unsupported agent/provider combination: %s/%s", agent, provider)
	}
}

// providerBaseURL returns provider's built-in default base URL, or "" if
// it doesn't have one. Currently only "ollama" (a well-known local
// backend) does; every other provider must supply its own via
// resolveBaseURL's explicit argument.
func providerBaseURL(provider string) string {
	if provider == "ollama" {
		return "http://localhost:11434/v1"
	}
	return ""
}

// resolveBaseURL picks the base URL to register for provider: the
// caller's explicit choice if given, otherwise provider's built-in
// default. A provider with neither (i.e. anything but "ollama" when the
// caller left -provider-base-url blank) is a configuration error, not a
// silently-empty base URL that would only surface later as a confusing
// connection failure inside the agent CLI itself.
func resolveBaseURL(provider, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if def := providerBaseURL(provider); def != "" {
		return def, nil
	}
	return "", fmt.Errorf("provider %q has no built-in default base URL; pass -provider-base-url (only \"ollama\" is built in)", provider)
}

// kiloCustomProviderNote is appended to a successful create/update
// response whenever a kilo instance is pointed at a non-ollama provider
// with an API key env var: CreateInstance has just written the registry
// fields that make agentmux's own project-level kilo.json regenerate with
// this provider on every session run, but that project-level config is
// where kilo refuses to accept "{env:VAR}" references at all — so the key
// itself still has to be wired up by hand, once, outside agentmux. See
// docs/custom-providers.md for the full explanation of why.
func kiloCustomProviderNote(provider, baseURL, model, apiKeyEnv string) string {
	return fmt.Sprintf(`
NOTE: kilo can't take an API key through agentmux-managed project config
(it rejects "{env:VAR}" references there). Two one-time steps, on this
host, are still needed before %[1]q can actually authenticate:

1. Add a provider block for %[1]q to kilo's shared global config
   (~/.config/kilo/kilo.jsonc — merges into every kilo project
   automatically), referencing %[4]s by name rather than embedding it:

     "provider": {
       %[1]q: {
         "name": %[1]q,
         "npm": "@ai-sdk/openai-compatible",
         "options": {
           "baseURL": %[2]q,
           "apiKey": "{env:%[4]s}"
         },
         "models": { %[3]q: { "name": %[3]q } }
       }
     }

2. Put the actual key in ~/.config/agentmux/kilo-env (created if absent),
   e.g.: %[4]s=sk-...
   agentmux injects this file's contents into every tmux-launched kilo
   instance's environment (kilo processes launched this way don't source
   a shell profile, so a plain "export" in .bashrc alone won't reach
   them — add it there too if you also want interactive 'kilo' sessions
   started directly from a terminal to pick it up).

Verify with: kilo roll-call %[3]s   (run from the instance's workdir)
See docs/custom-providers.md for more detail.`, provider, baseURL, model, apiKeyEnv)
}
