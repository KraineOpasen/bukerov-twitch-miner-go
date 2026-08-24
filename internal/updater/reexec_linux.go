//go:build linux

package updater

import "syscall"

func execReplacement(path string, args, env []string) error {
	return syscall.Exec(path, args, env)
}
