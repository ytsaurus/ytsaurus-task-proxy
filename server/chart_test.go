package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestChartTimeoutArguments(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart regression tests")
	}
	for _, tc := range []struct {
		name      string
		overrides map[string]any
		remove    []string
		wantAuth  string
		wantYT    string
	}{
		{"defaults", nil, nil, "-auth-check-timeout=800ms", "-yt-light-request-timeout=10s"},
		{"custom", map[string]any{"authCheckTimeout": "2.5s", "ytLightRequestTimeout": "25s"}, nil, "-auth-check-timeout=2.5s", "-yt-light-request-timeout=25s"},
		{"reuse previous release values", nil, []string{"authCheckTimeout", "ytLightRequestTimeout"}, "", ""},
		{"missing auth setting", nil, []string{"authCheckTimeout"}, "", "-yt-light-request-timeout=10s"},
		{"missing YT setting", nil, []string{"ytLightRequestTimeout"}, "-auth-check-timeout=800ms", ""},
		{"explicit empty", map[string]any{"authCheckTimeout": "", "ytLightRequestTimeout": ""}, nil, "-auth-check-timeout=", "-yt-light-request-timeout="},
		{"explicit zero", map[string]any{"authCheckTimeout": 0, "ytLightRequestTimeout": 0}, nil, "-auth-check-timeout=0", "-yt-light-request-timeout=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chart := filepath.Join(t.TempDir(), "chart")
			require.NoError(t, os.CopyFS(chart, os.DirFS("../chart")))
			valuesPath := filepath.Join(chart, "values.yaml")
			valuesBytes, err := os.ReadFile(valuesPath)
			require.NoError(t, err)
			var values map[string]any
			require.NoError(t, yaml.Unmarshal(valuesBytes, &values))
			timeouts := values["timeouts"].(map[string]any)
			for key, value := range tc.overrides {
				timeouts[key] = value
			}
			// --reuse-values renders the new templates with the previous chart's
			// defaults, which lack these newly introduced keys.
			for _, key := range tc.remove {
				delete(timeouts, key)
			}
			valuesBytes, err = yaml.Marshal(values)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(valuesPath, valuesBytes, 0600))
			out, err := exec.Command(helm, "template", "test", chart).CombinedOutput()
			require.NoError(t, err, "%s", out)
			decoder := yaml.NewDecoder(strings.NewReader(string(out)))
			var args []string
			for {
				var resource struct {
					Kind string `yaml:"kind"`
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Name string   `yaml:"name"`
									Args []string `yaml:"args"`
								} `yaml:"containers"`
							} `yaml:"spec"`
						} `yaml:"template"`
					} `yaml:"spec"`
				}
				if err := decoder.Decode(&resource); err == io.EOF {
					break
				} else {
					require.NoError(t, err)
				}
				if resource.Kind == "Deployment" {
					for _, container := range resource.Spec.Template.Spec.Containers {
						if container.Name == "server" {
							args = container.Args
						}
					}
				}
			}
			require.NotEmpty(t, args, "server container must be present")
			for prefix, want := range map[string]string{"-auth-check-timeout=": tc.wantAuth, "-yt-light-request-timeout=": tc.wantYT} {
				var got string
				for _, arg := range args {
					if strings.HasPrefix(arg, prefix) {
						got = arg
					}
				}
				require.Equal(t, want, got)
			}
		})
	}
}
