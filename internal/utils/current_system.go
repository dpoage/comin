package utils

import "os"

// CurrentSystem returns the store path /run/current-system currently
// points at, on NixOS and nix-darwin alike. It returns an empty string,
// with no error, when the symlink does not exist yet (e.g. comin has
// never deployed anything on this host).
func CurrentSystem() (string, error) {
	target, err := os.Readlink("/run/current-system")
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return target, nil
}
