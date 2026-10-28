package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every executor, darwin included, reads the current-system symlink.
func TestCurrentSystemIsTheSameOnEveryPlatform(t *testing.T) {
	link := filepath.Join(t.TempDir(), "current-system")
	assert.NoError(t, os.Symlink("/nix/store/aaaa-comin-current-system", link))

	old := currentSystem
	t.Cleanup(func() { currentSystem = old })
	currentSystem = func() (string, error) {
		return os.Readlink(link)
	}

	nix, _ := NewNixExecutor()
	nixos, _ := NewNixFlakeExecutor("nixosConfigurations")
	darwin, _ := NewNixFlakeExecutor("darwinConfigurations")

	for name, e := range map[string]Executor{"nix": nix, "nixos flake": nixos, "darwin flake": darwin} {
		got, err := e.CurrentSystem()
		assert.NoError(t, err, name)
		assert.Equal(t, "/nix/store/aaaa-comin-current-system", got, name)
	}
}
