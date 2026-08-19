// Package wizardui owns the instance-creation forms shared by the live
// wizard and the deterministic UX screenshot generator.
package wizardui

import (
	"strings"

	"github.com/charmbracelet/huh"
)

// Capabilities identifies the settings that are meaningful for one agent.
type Capabilities struct {
	DisplayHostName bool
	Provider        bool
	ProviderAPIKey  bool
	Compact         bool
}

// CapabilitiesForAgent returns the settings the details form should expose.
func CapabilitiesForAgent(agent string) Capabilities {
	switch agent {
	case "claude-code":
		return Capabilities{DisplayHostName: true, Compact: true}
	case "kilo":
		return Capabilities{DisplayHostName: true, Provider: true, ProviderAPIKey: true}
	case "zero", "opencode":
		return Capabilities{Provider: true}
	default:
		return Capabilities{}
	}
}

// DefaultInstance returns the visible instance-name default for an agent.
func DefaultInstance(agent string) string {
	if agent == "claude-code" {
		return "claude-code"
	}
	return "agentmux"
}

// UsesDefaultProvider reports whether custom-provider fields should be hidden.
func UsesDefaultProvider(provider string) bool {
	provider = strings.TrimSpace(provider)
	return provider == "" || provider == "ollama"
}

// Details holds all mutable answers bound to the agent-specific form.
type Details struct {
	Instance          string
	RunUser           string
	HostName          string
	Workdir           string
	Provider          string
	Model             string
	ProviderBaseURL   string
	ProviderAPIKeyEnv string
	CompactOnUpdate   string
	SetupDiscord      bool
}

// NewSelectionForm creates the first wizard step: where and what to create.
func NewSelectionForm(hostNames []string, host, agent *string) *huh.Form {
	hostOptions := make([]huh.Option[string], len(hostNames))
	for i, name := range hostNames {
		hostOptions[i] = huh.NewOption(name, name)
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Device").Options(hostOptions...).Value(host),
		huh.NewSelect[string]().Title("Agent").
			Options(
				huh.NewOption("claude-code", "claude-code"),
				huh.NewOption("zero", "zero"),
				huh.NewOption("opencode", "opencode"),
				huh.NewOption("kilo", "kilo"),
			).
			Value(agent),
	))
}

// DetailsGroups builds the exact groups used by the live details form. It is
// exported separately so UX screenshots can focus one real group at a time.
func DetailsGroups(agent string, details *Details, includeDiscord bool) []*huh.Group {
	capabilities := CapabilitiesForAgent(agent)
	commonFields := []huh.Field{
		huh.NewInput().Title("Instance name").Value(&details.Instance),
		huh.NewInput().Title("Run as user").Description("required on Linux; ignored on macOS").Value(&details.RunUser),
	}
	if capabilities.DisplayHostName {
		commonFields = append(commonFields,
			huh.NewInput().Title("Host name").Value(&details.HostName),
		)
	}
	commonFields = append(commonFields,
		huh.NewInput().Title("Workdir").Description("blank = provisioner default").Value(&details.Workdir),
	)
	groups := []*huh.Group{huh.NewGroup(commonFields...)}

	if capabilities.Provider {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().Title("Provider").Description("ollama, or a custom OpenAI-compatible provider id").Value(&details.Provider),
			huh.NewInput().Title("Model").Description("blank = provisioner default").Value(&details.Model),
		))

		customProviderFields := []huh.Field{
			huh.NewInput().Title("Provider base URL").Description("required for a custom provider").Value(&details.ProviderBaseURL),
		}
		if capabilities.ProviderAPIKey {
			customProviderFields = append(customProviderFields,
				huh.NewInput().Title("Provider API key env var").Description("optional; env var name only, never the key itself").Value(&details.ProviderAPIKeyEnv),
			)
		}
		groups = append(groups, huh.NewGroup(customProviderFields...).WithHideFunc(func() bool {
			return UsesDefaultProvider(details.Provider)
		}))
	}

	if capabilities.Compact {
		groups = append(groups, huh.NewGroup(
			huh.NewSelect[string]().Title("Compact before nightly resume?").
				Description("prevents Claude Code's huge-session resume prompt by compacting before the nightly restart").
				Options(
					huh.NewOption("on (default)", ""),
					huh.NewOption("off", "off"),
				).
				Value(&details.CompactOnUpdate),
		))
	}

	if includeDiscord {
		groups = append(groups, huh.NewGroup(
			huh.NewConfirm().
				Title("Set up Discord notifications?").
				Description("Optional — lets agentmux message you proactively (e.g. a Claude Code refresh token about to expire). Can also be done later with 'agentmux notify discord setup'.").
				Value(&details.SetupDiscord),
		))
	}
	return groups
}

// NewDetailsForm creates the live agent-specific details form.
func NewDetailsForm(agent string, details *Details, includeDiscord bool) *huh.Form {
	return huh.NewForm(DetailsGroups(agent, details, includeDiscord)...)
}
