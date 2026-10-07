package chart_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	taskproxy "github.com/ytsaurus/ytsaurus-task-proxy/pkg"
	"gopkg.in/yaml.v3"
)

func render(t *testing.T, values string) (map[string]map[string]any, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(path, []byte(values), 0600))
	command := exec.Command("helm", "template", "logs", "../../chart", "--namespace", "tests", "-f", path)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, &renderError{string(output)}
	}
	documents := map[string]map[string]any{}
	decoder := yaml.NewDecoder(strings.NewReader(string(output)))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(doc) == 0 {
			continue
		}
		metadata := doc["metadata"].(map[string]any)
		documents[doc["kind"].(string)+"/"+metadata["name"].(string)] = doc
	}
	return documents, nil
}

type renderError struct{ output string }

func (e *renderError) Error() string   { return e.output }
func mapping(value any) map[string]any { return value.(map[string]any) }
func container(pod map[string]any, name string) map[string]any {
	for _, c := range pod["containers"].([]any) {
		m := mapping(c)
		if m["name"] == name {
			return m
		}
	}
	panic(name)
}
func mounted(c map[string]any, name, path string) bool {
	for _, v := range c["volumeMounts"].([]any) {
		m := mapping(v)
		if m["name"] == name && m["mountPath"] == path {
			_, subpath := m["subPath"]
			return !subpath
		}
	}
	return false
}
func runtimeConfig(t *testing.T, docs map[string]map[string]any) taskproxy.LoggingConfig {
	t.Helper()
	require.NotContains(t, mapping(docs["ConfigMap/logs-config"]["metadata"]), "logging.yaml", "logging content belongs only in ConfigMap data")
	data := mapping(docs["ConfigMap/logs-config"]["data"])
	content, ok := data["logging.yaml"].(string)
	require.True(t, ok, "render logging YAML for runtime")
	path := filepath.Join(t.TempDir(), "logging.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	cfg, err := taskproxy.LoadLoggingConfig(path)
	require.NoError(t, err)
	return cfg
}
func TestChartLoggingAndIndependentPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, values, server, proxy, access string
	}{
		{"default", "", "stderr", "stderr", "stderr"},
		{"stdout", "server:\n  logging:\n    writerType: stdout\nproxy:\n  logging:\n    writerType: stdout\n  accessLog:\n    writerType: stdout\n", "stdout", "stdout", "stdout"},
		{"server file", "server:\n  logging:\n    writerType: file\n", "file", "stderr", "stderr"},
		{"envoy file", "proxy:\n  logging:\n    writerType: file\n", "stderr", "file", "stderr"},
		{"access file", "proxy:\n  accessLog:\n    writerType: file\n", "stderr", "stderr", "file"},
		{"all files TLS replicas", "replicas: 3\ntls:\n  enabled: true\nserver:\n  logging:\n    writerType: file\nproxy:\n  logging:\n    writerType: file\n  accessLog:\n    writerType: file\n", "file", "file", "file"},
		{"disabled access", "proxy:\n  accessLog:\n    enabled: false\n    writerType: file\n", "stderr", "stderr", "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := render(t, tc.values)
			require.NoError(t, err)
			sts, ok := docs["StatefulSet/logs"]
			require.True(t, ok, "use StatefulSet for stable replica identity")
			require.Nil(t, docs["Deployment/logs"])
			spec := mapping(sts["spec"])
			require.Equal(t, "logs-headless", spec["serviceName"])
			headless := mapping(docs["Service/logs-headless"]["spec"])
			require.Equal(t, "None", headless["clusterIP"])
			require.Equal(t, "logs", mapping(headless["selector"])["app.kubernetes.io/instance"])
			template := mapping(spec["template"])
			require.NotEmpty(t, mapping(mapping(template["metadata"])["annotations"])["checksum/config"])
			pod := mapping(template["spec"])
			server := container(pod, "server")
			envoy := container(pod, "envoy")
			require.True(t, mounted(server, "logging-config", "/etc/task-proxy"))
			require.Contains(t, server["args"].([]any), "-logging-config=/etc/task-proxy/logging.yaml")
			cfg := runtimeConfig(t, docs)
			require.Equal(t, tc.server, cfg.Server.WriterType)
			require.Equal(t, tc.proxy, cfg.Proxy.WriterType)
			require.Equal(t, tc.access, cfg.AccessLog.WriterType)
			args := envoy["args"].([]any)
			require.Contains(t, args, "--log-level")
			require.Contains(t, args, cfg.Proxy.MinLogLevel)
			if tc.proxy != "stderr" {
				require.Contains(t, args, "--log-path")
				require.Contains(t, args, cfg.ProxyLogPath())
			} else {
				require.NotContains(t, args, "--log-path")
			}
			{
				claims := spec["volumeClaimTemplates"].([]any)
				require.Len(t, claims, 1)
				claim := mapping(claims[0])
				require.Equal(t, "logs", mapping(claim["metadata"])["name"])
				require.Equal(t, []any{"ReadWriteOnce"}, mapping(claim["spec"])["accessModes"])
				require.Equal(t, "5Gi", mapping(mapping(mapping(claim["spec"])["resources"])["requests"])["storage"])
				require.Equal(t, 101, mapping(pod["securityContext"])["fsGroup"])
				require.True(t, mounted(server, "logs", taskproxy.DefaultLogDirectory))
				require.True(t, mounted(envoy, "logs", taskproxy.DefaultLogDirectory))
				require.Equal(t, "Retain", mapping(spec["persistentVolumeClaimRetentionPolicy"])["whenDeleted"])
				require.Equal(t, "Retain", mapping(spec["persistentVolumeClaimRetentionPolicy"])["whenScaled"])
				for _, p := range []*taskproxy.RotationPolicy{cfg.Server.RotationPolicy, cfg.Proxy.RotationPolicy, cfg.AccessLog.RotationPolicy} {
					if p != nil {
						require.EqualValues(t, 3600000, p.RotationPeriodMilliseconds)
						require.EqualValues(t, 100*1024*1024, p.MaxSegmentSize)
						require.EqualValues(t, 1024*1024*1024, p.MaxTotalSizeToKeep)
						require.EqualValues(t, 10, p.MaxSegmentCountToKeep)
					}
				}
			}
			if tc.name == "all files TLS replicas" {
				require.Equal(t, 3, spec["replicas"])
				require.True(t, mounted(envoy, "cert", "/etc/certs"))
				require.Equal(t, "LoadBalancer", mapping(docs["Service/logs"]["spec"])["type"])
			}
		})
	}
}
func TestChartCustomPolicyAndConfigChecksum(t *testing.T) {
	docs, err := render(t, "server:\n  logging:\n    writerType: file\n    rotationPolicy:\n      maxSegmentSize: 8Mi\n      maxSegmentCountToKeep: 3\npersistence:\n  storageClass: fast\n  size: 2Gi\n")
	require.NoError(t, err)
	cfg := runtimeConfig(t, docs)
	require.EqualValues(t, 0, cfg.Server.RotationPolicy.RotationPeriodMilliseconds)
	require.EqualValues(t, 8*1024*1024, cfg.Server.RotationPolicy.MaxSegmentSize)
	require.EqualValues(t, 3, cfg.Server.RotationPolicy.MaxSegmentCountToKeep)
	spec := mapping(docs["StatefulSet/logs"]["spec"])
	require.Equal(t, "fast", mapping(mapping(spec["volumeClaimTemplates"].([]any)[0])["spec"])["storageClassName"])
	defaults, err := render(t, "")
	require.NoError(t, err)
	annotation := func(d map[string]map[string]any) any {
		return mapping(mapping(mapping(mapping(d["StatefulSet/logs"]["spec"])["template"])["metadata"])["annotations"])["checksum/config"]
	}
	require.NotEqual(t, annotation(defaults), annotation(docs))
}
func TestChartRejectsInvalidLogging(t *testing.T) {
	for _, values := range []string{
		"server:\n  logging:\n    writerType: bad\n", "proxy:\n  logging:\n    minLogLevel: warn\n", "proxy:\n  accessLog:\n    minLogLevel: info\n",
		"server:\n  logging:\n    rotationPolicy:\n      maxSegmentSize: 10Mi\n      maxSegmentCountToKeep: 2\n",
		"server:\n  logging:\n    writerType: file\n    rotationPolicy:\n      maxSegmentSize: 10Mi\n",
		"server:\n  logging:\n    writerType: file\n    rotationPolicy:\n      maxSegmentCountToKeep: 2\n",
		"server:\n  logging:\n    writerType: file\n    rotationPolicy:\n      rotationPeriodMilliseconds: 0\n      maxSegmentCountToKeep: 2\n",
		"server:\n  logging:\n    writerType: file\n    rotationPolicy:\n      maxSegmentSize: -1\n      maxSegmentCountToKeep: 2\n",
		"server:\n  logging:\n    writerType: file\n    rotationPolicy:\n      maxSegmentSize: 8Mi\n      maxSegmentCountToKeep: 2\n      unknown: 1\n",
		"persistence:\n  existingClaim: shared\n",
	} {
		t.Run(strings.ReplaceAll(values, "\n", "/"), func(t *testing.T) { _, err := render(t, values); require.Error(t, err) })
	}
}

func TestChartLoggingToggleKeepsClaimsAndStorageClassSemantics(t *testing.T) {
	defaults, err := render(t, "")
	require.NoError(t, err)
	files, err := render(t, "server:\n  logging:\n    writerType: file\n")
	require.NoError(t, err)
	defaultClaims := mapping(defaults["StatefulSet/logs"]["spec"])["volumeClaimTemplates"]
	require.Equal(t, defaultClaims, mapping(files["StatefulSet/logs"]["spec"])["volumeClaimTemplates"], "StatefulSet claim templates must remain unchanged when logging mode toggles")
	_, exists := mapping(mapping(defaultClaims.([]any)[0])["spec"])["storageClassName"]
	require.False(t, exists, "null uses cluster default")
	empty, err := render(t, "persistence:\n  storageClass: \"\"\n")
	require.NoError(t, err)
	require.Equal(t, "", mapping(mapping(mapping(empty["StatefulSet/logs"]["spec"])["volumeClaimTemplates"].([]any)[0])["spec"])["storageClassName"])
	cfg := runtimeConfig(t, files)
	require.True(t, cfg.AccessLog.Enabled)
	disabled, err := render(t, "proxy:\n  accessLog:\n    enabled: false\n")
	require.NoError(t, err)
	require.False(t, runtimeConfig(t, disabled).AccessLog.Enabled, "explicit false must remain false")
}
