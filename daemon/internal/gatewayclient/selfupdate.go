package gatewayclient

import (
	"context"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
)

// ShipPublish records the orchestrator's shipped commit per repo in the
// host's ship gate file; the pull updater installs up to that commit.
func (c *Client) ShipPublish(ctx context.Context, commits map[string]string) (gatewayapi.ShipPublishResponse, error) {
	var out gatewayapi.ShipPublishResponse
	err := c.call(ctx, gatewayapi.OpShipPublish, QueryTimeout, gatewayapi.ShipPublishRequest{Commits: commits}, &out)
	return out, err
}

// Versions reports the installed commit per repo on the host.
func (c *Client) Versions(ctx context.Context) (gatewayapi.VersionsResponse, error) {
	var out gatewayapi.VersionsResponse
	err := c.call(ctx, gatewayapi.OpVersions, QueryTimeout, gatewayapi.VersionsRequest{}, &out)
	return out, err
}

// SelfUpdateLog tails the host updater's event log.
func (c *Client) SelfUpdateLog(ctx context.Context, lines int) (gatewayapi.SelfUpdateLogResponse, error) {
	var out gatewayapi.SelfUpdateLogResponse
	err := c.call(ctx, gatewayapi.OpSelfUpdateLog, QueryTimeout, gatewayapi.SelfUpdateLogRequest{Lines: lines}, &out)
	return out, err
}
