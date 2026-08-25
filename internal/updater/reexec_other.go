//go:build !linux

package updater

import "fmt"

func execReplacement(_ string, _, _ []string) error {
	return fmt.Errorf("stable recovery re-exec is supported only on linux")
}
