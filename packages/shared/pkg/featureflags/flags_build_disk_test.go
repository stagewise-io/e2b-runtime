package featureflags

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	"github.com/launchdarkly/go-server-sdk/v7/testhelpers/ldtestdata"
	"github.com/stretchr/testify/require"
)

func TestBuildEnsureFreeDiskSpaceEnv(t *testing.T) {
	t.Parallel()

	// Flags register their offline defaults during package initialization. A
	// subprocess tests the actual declaration, not a copy constructed after Setenv.
	if expected := os.Getenv("TEST_BUILD_DISK_EXPECTED"); expected != "" {
		want, err := strconv.ParseBool(expected)
		require.NoError(t, err)
		require.Equal(t, want, BuildEnsureFreeDiskSpace.Fallback())

		client, err := NewClientWithDatasource(launchDarklyOfflineStore)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, client.Close(t.Context())) })
		require.Equal(t, want, client.BoolFlag(t.Context(), BuildEnsureFreeDiskSpace))

		// An evaluated flag still wins over the environment fallback.
		data := ldtestdata.DataSource()
		data.Update(data.Flag(BuildEnsureFreeDiskSpace.Key()).ValueForAll(ldvalue.Bool(!want)))
		remote, err := NewClientWithDatasource(data)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, remote.Close(t.Context())) })
		require.Equal(t, !want, remote.BoolFlag(t.Context(), BuildEnsureFreeDiskSpace))

		return
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset"},
		{name: "empty", set: true},
		{name: "enabled", set: true, value: "true", want: true},
		{name: "disabled", set: true, value: "false"},
		{name: "invalid stays disabled", set: true, value: "enable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestBuildEnsureFreeDiskSpaceEnv$")
			cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
				return strings.HasPrefix(entry, "BUILD_ENSURE_FREE_DISK_SPACE=") ||
					strings.HasPrefix(entry, "TEST_BUILD_DISK_EXPECTED=")
			})
			cmd.Env = append(cmd.Env, "TEST_BUILD_DISK_EXPECTED="+strconv.FormatBool(tc.want))
			if tc.set {
				cmd.Env = append(cmd.Env, "BUILD_ENSURE_FREE_DISK_SPACE="+tc.value)
			}
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}
