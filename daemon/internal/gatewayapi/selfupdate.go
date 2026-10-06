package gatewayapi

// Self-update operations, as named in the app capability's "ops" lists
// (see AMUX-29). They let the hub publish the shipped commit per repo
// and read back what the Mac runs, without any access into the Mac.
const (
	// OpShipPublish records the orchestrator's shipped commit per repo
	// in the Mac's ship gate file. The updater installs up to that
	// commit and no further.
	OpShipPublish = "ship-publish"
	// OpVersions reports the installed commit per repo from the Mac's
	// versions file, so the orchestrator can confirm the Mac is on a
	// commit or warn when it lags.
	OpVersions = "versions"
	// OpSelfUpdateLog tails the Mac updater's own event log, including
	// the `deployed <repo>@<sha>` line on success.
	OpSelfUpdateLog = "selfupdate-log"
)

// SelfUpdateOps lists the self-update operations.
var SelfUpdateOps = []string{OpShipPublish, OpVersions, OpSelfUpdateLog}

// ShipPublishRequest records shipped commits: repo name to full commit
// sha. Unknown repos and malformed shas are ignored, never installed.
type ShipPublishRequest struct {
	Commits map[string]string `json:"commits"`
}

// ShipPublishResponse is the gate after the publish merged.
type ShipPublishResponse struct {
	Shipped map[string]string `json:"shipped"`
}

// VersionsRequest asks what the host runs. Empty: the whole record.
type VersionsRequest struct{}

// VersionsResponse is the installed commit per repo plus the host, so
// the orchestrator can confirm "laptop is on <sha>".
type VersionsResponse struct {
	Host     string            `json:"host"`
	Versions map[string]string `json:"versions"`
}

// SelfUpdateLogRequest tails the updater log. Lines caps the reply.
type SelfUpdateLogRequest struct {
	Lines int `json:"lines,omitempty"`
}

// SelfUpdateLogResponse is the tail of the updater log.
type SelfUpdateLogResponse struct {
	Lines []string `json:"lines"`
}
