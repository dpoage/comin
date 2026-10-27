package executor

import (
	"testing"

	"github.com/nlewo/comin/internal/utils"
	"github.com/stretchr/testify/assert"
)

// Every executor, darwin included, reports what utils.CurrentSystem reads.
func TestCurrentSystemIsTheSameOnEveryPlatform(t *testing.T) {
	want, wantErr := utils.CurrentSystem()

	nix, _ := NewNixExecutor()
	nixos, _ := NewNixFlakeExecutor("nixosConfigurations")
	darwin, _ := NewNixFlakeExecutor("darwinConfigurations")

	for name, e := range map[string]Executor{"nix": nix, "nixos flake": nixos, "darwin flake": darwin} {
		got, err := e.CurrentSystem()
		assert.Equal(t, want, got, name)
		assert.Equal(t, wantErr, err, name)
	}
}
