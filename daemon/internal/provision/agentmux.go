package provision

import "fmt"

const (
	defaultAgentmuxInstance = "agentmux"
	defaultOllamaModel      = "gpt-oss:20b-cloud"
	defaultProviderWaitSecs = "60"
)

func validateSupportedAgentProvider(agent, provider string) error {
	switch agent + ":" + provider {
	case "zero:ollama", "opencode:ollama":
		return nil
	default:
		return fmt.Errorf("unsupported agent/provider combination: %s/%s", agent, provider)
	}
}

func providerBaseURL(provider string) string {
	if provider == "ollama" {
		return "http://localhost:11434/v1"
	}
	return ""
}
