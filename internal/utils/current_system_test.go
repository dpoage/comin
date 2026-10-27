package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func setCurrentSystemPath(t *testing.T, path string) {
	t.Helper()
	old := currentSystemPath
	t.Cleanup(func() { currentSystemPath = old })
	currentSystemPath = path
}

// CurrentSystem reads the same symlink on NixOS and nix-darwin.
func TestCurrentSystemReadsTheSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "current-system")
	assert.NoError(t, os.Symlink("/nix/store/aaaa-nixos-system", link))
	setCurrentSystemPath(t, link)

	got, err := CurrentSystem()
	assert.NoError(t, err)
	assert.Equal(t, "/nix/store/aaaa-nixos-system", got)
}

func TestCurrentSystemWithoutTheSymlinkIsUnknownNotAnError(t *testing.T) {
	setCurrentSystemPath(t, filepath.Join(t.TempDir(), "absent"))

	got, err := CurrentSystem()
	assert.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestCurrentSystemReportsAnUnreadableLink(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-symlink")
	assert.NoError(t, os.WriteFile(file, nil, 0o600))
	setCurrentSystemPath(t, file)

	got, err := CurrentSystem()
	assert.Error(t, err)
	assert.Equal(t, "", got)
}
