package main

import (
	"flag"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerStartupTimeouts(t *testing.T) {
	if os.Getenv("TASK_PROXY_TEST_STARTUP") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"server"}, os.Args[i+1:]...)
				break
			}
		}
		flag.CommandLine = flag.NewFlagSet("server", flag.ExitOnError)
		main()
		return
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"defaults", nil, "auth-check-timeout=800ms, yt-light-request-timeout=10s"},
		{"auth override", []string{"-auth-check-timeout=2.5s"}, "auth-check-timeout=2.5s, yt-light-request-timeout=10s"},
		{"YT override", []string{"-yt-light-request-timeout=25s"}, "auth-check-timeout=800ms, yt-light-request-timeout=25s"},
		{"auth zero", []string{"-auth-check-timeout=0s"}, "auth-check-timeout must be positive"},
		{"auth negative", []string{"-auth-check-timeout=-1s"}, "auth-check-timeout must be positive"},
		{"YT zero", []string{"-yt-light-request-timeout=0"}, "yt-light-request-timeout must be positive"},
		{"YT negative", []string{"-yt-light-request-timeout=-1s"}, "yt-light-request-timeout must be positive"},
		{"auth empty", []string{"-auth-check-timeout="}, "parse error"},
		{"YT empty", []string{"-yt-light-request-timeout="}, "parse error"},
		{"auth missing unit", []string{"-auth-check-timeout=800"}, `invalid value "800" for flag -auth-check-timeout`},
		{"YT malformed", []string{"-yt-light-request-timeout=bad"}, "parse error"},
		{"auth overflow", []string{"-auth-check-timeout=999999999999999999s"}, "parse error"},
		{"YT overflow", []string{"-yt-light-request-timeout=999999999999999999s"}, "parse error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-test.run=^TestServerStartupTimeouts$", "--", "-yt-proxy=unused", "-yt-token-path=" + t.TempDir() + "/missing-token", "-base-domain=example.com", "-dir-path=//tmp/task-proxy"}
			cmd := exec.Command(os.Args[0], append(args, tc.args...)...)
			cmd.Env = append(os.Environ(), "TASK_PROXY_TEST_STARTUP=1")
			out, err := cmd.CombinedOutput()
			require.Error(t, err, "startup must stop before any YT access")
			require.Contains(t, string(out), tc.want)
		})
	}
}
