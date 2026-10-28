package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

// switchScript rewrites the observed unit definition, then exits with exitCode.
func switchScript(unitFile string, changeUnit bool, exitCode int) string {
	body := ""
	if changeUnit {
		body = fmt.Sprintf("echo new > %q\n", unitFile)
	}
	return body + fmt.Sprintf("exit %d", exitCode)
}

func newFakeUnit(t *testing.T) string {
	t.Helper()
	unitFile := filepath.Join(t.TempDir(), "comin.service")
	require.NoError(t, os.WriteFile(unitFile, []byte("old\n"), 0o644))
	return unitFile
}

func unitFileHash(unitFile string) func() string {
	return func() string {
		b, err := os.ReadFile(unitFile)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

type deployCase struct {
	name        string
	changeUnit  bool
	switchFails bool
}

var deployCases = []deployCase{
	{"switch fails, unit changed", true, true},
	{"switch fails, unit unchanged", false, true},
	{"switch succeeds, unit changed", true, false},
	{"switch succeeds, unit unchanged", false, false},
}

func exitCode(fails bool) int {
	if fails {
		return 1
	}
	return 0
}

func TestDeployWithLinux(t *testing.T) {
	for _, tc := range deployCases {
		t.Run(tc.name, func(t *testing.T) {
			unitFile := newFakeUnit(t)
			outPath := t.TempDir()
			exe := filepath.Join(outPath, "bin", "switch-to-configuration")
			writeScript(t, exe, switchScript(unitFile, tc.changeUnit, exitCode(tc.switchFails)))

			restart, profilePath, err := deployWith(outPath, "test", unitFileHash(unitFile), switchToConfigurationLinux)

			assert.Equal(t, tc.changeUnit, restart)
			assert.Empty(t, profilePath)
			if tc.switchFails {
				assert.EqualError(t, err, fmt.Sprintf("command %s test fails with exit status 1", exe))
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDeployWithDarwin(t *testing.T) {
	for _, tc := range deployCases {
		t.Run(tc.name, func(t *testing.T) {
			unitFile := newFakeUnit(t)
			outPath := t.TempDir()
			writeScript(t, filepath.Join(outPath, "activate-user"), "exit 0")
			exe := filepath.Join(outPath, "activate")
			writeScript(t, exe, switchScript(unitFile, tc.changeUnit, exitCode(tc.switchFails)))

			restart, profilePath, err := deployWith(outPath, "test", unitFileHash(unitFile), switchToConfigurationDarwin)

			assert.Equal(t, tc.changeUnit, restart)
			assert.Empty(t, profilePath)
			if tc.switchFails {
				assert.EqualError(t, err, fmt.Sprintf("system activation command %s fails with exit status 1", exe))
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// deployLinux must hash the unit through `systemctl cat comin.service`.
func TestDeployLinuxHashesSystemctlUnit(t *testing.T) {
	for _, tc := range deployCases {
		t.Run(tc.name, func(t *testing.T) {
			unitFile := newFakeUnit(t)
			binDir := t.TempDir()
			writeScript(t, filepath.Join(binDir, "systemctl"), fmt.Sprintf("cat %q", unitFile))
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			outPath := t.TempDir()
			writeScript(t, filepath.Join(outPath, "bin", "switch-to-configuration"),
				switchScript(unitFile, tc.changeUnit, exitCode(tc.switchFails)))

			restart, _, err := deployLinux(context.Background(), outPath, "test")

			assert.Equal(t, tc.changeUnit, restart)
			assert.Equal(t, tc.switchFails, err != nil)
		})
	}
}
