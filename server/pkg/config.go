package pkg

import (
	"context"
	"fmt"
	"time"

	"go.ytsaurus.tech/yt/go/ypath"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

// TaskProxyConfig publishes the base domain derived from the server's flags.
// It is discovery metadata, not a source of server configuration.
type TaskProxyConfig struct {
	BaseDomain string `yson:"base_domain"`
}

// WriteConfig prepares discovery metadata before the server starts serving.
// Existing service rows are preserved; the config document is refreshed on each start.
func WriteConfig(ctx context.Context, yt ytsdk.Client, dirPath string, config TaskProxyConfig) error {
	dir := ypath.Path(dirPath)
	if err := createCypressNode(ctx, yt, dir.Child(servicesTableName), ytsdk.NodeTable); err != nil {
		return err
	}
	configPath := dir.Child("config")
	if err := createCypressNode(ctx, yt, configPath, ytsdk.NodeDocument); err != nil {
		return err
	}
	started := time.Now()
	err := yt.SetNode(ctx, configPath, config, nil)
	defaultMetrics.ObserveYTDuration("set_node", time.Since(started))
	if err != nil {
		defaultMetrics.ObserveYTError("set_node", err)
		return fmt.Errorf("failed to publish task proxy config at %q: %w", configPath, err)
	}
	return nil
}

func createCypressNode(ctx context.Context, yt ytsdk.Client, path ypath.Path, nodeType ytsdk.NodeType) error {
	started := time.Now()
	_, err := yt.CreateNode(ctx, path, nodeType, &ytsdk.CreateNodeOptions{
		Recursive:      true,
		IgnoreExisting: true,
	})
	defaultMetrics.ObserveYTDuration("create_node", time.Since(started))
	if err != nil {
		defaultMetrics.ObserveYTError("create_node", err)
		return fmt.Errorf("failed to create task proxy %s at %q: %w", nodeType, path, err)
	}
	return nil
}
