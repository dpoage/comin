package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A boot deployment recorded in store.json before config load rejected
// boot still needs a reboot while its out path is not the current system.
func TestNeedToRebootLinuxPersistedBootDeployment(t *testing.T) {
	link := filepath.Join(t.TempDir(), "current-system")
	assert.NoError(t, os.Symlink("/nix/store/aaaa-nixos-system-current", link))
	setCurrentSystemPath(t, link)

	assert.True(t, NeedToRebootLinux("/nix/store/zzzz-nixos-system-pending", "boot"))
}
