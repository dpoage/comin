package utils

import "os"

// CurrentSystemLinux returns the store path /run/current-system currently
// points at. It returns an empty string, with no error, when the symlink
// does not exist yet (e.g. comin has never deployed anything on this host).
func CurrentSystemLinux() (string, error) {
	target, err := os.Readlink("/run/current-system")
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return target, nil
}

// CurrentSystemDarwin has no equivalent of /run/current-system: nix-darwin
// activation does not expose a single "currently active" symlink comin can
// compare against. It reports unknown rather than guessing.
func CurrentSystemDarwin() (string, error) {
	return "", nil
}
