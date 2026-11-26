package pkg

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.ytsaurus.tech/yt/go/ypath"
	"go.ytsaurus.tech/yt/go/yson"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

type cypressNode struct {
	nodeType ytsdk.NodeType
	value    []byte
}

type configYT struct {
	ytsdk.Client
	nodes      map[ypath.Path]cypressNode
	failCreate ypath.Path
	failSet    bool
	err        error
}

func (c *configYT) CreateNode(_ context.Context, path ypath.YPath, nodeType ytsdk.NodeType, opts *ytsdk.CreateNodeOptions) (ytsdk.NodeID, error) {
	p := path.(ypath.Path)
	if p == c.failCreate {
		return ytsdk.NodeID{}, c.err
	}
	if node, exists := c.nodes[p]; exists {
		if opts == nil || !opts.IgnoreExisting || node.nodeType != nodeType {
			return ytsdk.NodeID{}, fmt.Errorf("node already exists: %s", p)
		}
		return ytsdk.NodeID{}, nil
	}
	parent, _, err := ypath.Split(p)
	if err != nil {
		return ytsdk.NodeID{}, err
	}
	if _, exists := c.nodes[parent]; !exists {
		if opts == nil || !opts.Recursive {
			return ytsdk.NodeID{}, fmt.Errorf("parent does not exist: %s", parent)
		}
		c.nodes[parent] = cypressNode{nodeType: ytsdk.NodeMap}
	}
	c.nodes[p] = cypressNode{nodeType: nodeType}
	return ytsdk.NodeID{}, nil
}

func (c *configYT) SetNode(_ context.Context, path ypath.YPath, value any, _ *ytsdk.SetNodeOptions) error {
	if c.failSet {
		return c.err
	}
	p := path.(ypath.Path)
	node, exists := c.nodes[p]
	if !exists || node.nodeType != ytsdk.NodeDocument {
		return fmt.Errorf("document does not exist: %s", p)
	}
	data, err := yson.Marshal(value)
	if err != nil {
		return err
	}
	node.value = data
	c.nodes[p] = node
	return nil
}

func TestWriteConfigPublishesConfigBeforeDiscovery(t *testing.T) {
	for _, dir := range []string{"//sys/task_proxies", "//custom/proxy"} {
		t.Run(dir, func(t *testing.T) {
			c := &configYT{nodes: make(map[ypath.Path]cypressNode)}
			config := TaskProxyConfig{
				BaseDomain: "tasks.example.com",
			}
			require.NoError(t, WriteConfig(context.Background(), c, dir, config))
			path := ypath.Path(dir)
			require.Equal(t, ytsdk.NodeMap, c.nodes[path].nodeType)
			require.Equal(t, ytsdk.NodeTable, c.nodes[path.Child("services")].nodeType)
			require.Equal(t, ytsdk.NodeDocument, c.nodes[path.Child("config")].nodeType)
			var published map[string]any
			require.NoError(t, yson.Unmarshal(c.nodes[path.Child("config")].value, &published))
			require.Equal(t, map[string]any{
				"base_domain": "tasks.example.com",
			}, published)
		})
	}
}

func TestWriteConfigRefreshesConfigAndPreservesServices(t *testing.T) {
	c := &configYT{nodes: make(map[ypath.Path]cypressNode)}
	ctx := context.Background()
	require.NoError(t, WriteConfig(ctx, c, "//sys/task_proxies", TaskProxyConfig{
		BaseDomain: "old.example.com",
	}))
	servicesPath := ypath.Path("//sys/task_proxies/services")
	services := c.nodes[servicesPath]
	services.value = []byte("existing service rows")
	c.nodes[servicesPath] = services

	config := TaskProxyConfig{BaseDomain: "new.example.com"}
	require.NoError(t, WriteConfig(ctx, c, "//sys/task_proxies", config))
	require.Equal(t, services, c.nodes[servicesPath])
	var published map[string]any
	require.NoError(t, yson.Unmarshal(c.nodes["//sys/task_proxies/config"].value, &published))
	require.Equal(t, map[string]any{
		"base_domain": "new.example.com",
	}, published)
}

func TestWriteConfigReportsFailures(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failCreate ypath.Path
		failSet    bool
		message    string
	}{
		{"services creation", "//sys/task_proxies/services", false, "failed to create task proxy table"},
		{"config creation", "//sys/task_proxies/config", false, "failed to create task proxy document"},
		{"config publication", "", true, "failed to publish task proxy config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cause := errors.New("YT unavailable")
			c := &configYT{
				nodes: make(map[ypath.Path]cypressNode), failCreate: tt.failCreate, failSet: tt.failSet, err: cause,
			}
			err := WriteConfig(context.Background(), c, "//sys/task_proxies", TaskProxyConfig{BaseDomain: "example.com"})
			require.ErrorIs(t, err, cause)
			require.ErrorContains(t, err, tt.message)
		})
	}
}
