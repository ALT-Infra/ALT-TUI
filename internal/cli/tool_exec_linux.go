//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func (s *commandState) toolExecCommand() *cobra.Command {
	var workspace string
	var temp string
	var inner bool
	command := &cobra.Command{
		Use:    "__tool-exec --workspace DIR --temp DIR -- COMMAND [ARG...]",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := physicalDirectory(workspace)
			if err != nil {
				return fmt.Errorf("sandbox workspace: %w", err)
			}
			privateTemp, err := physicalDirectory(temp)
			if err != nil {
				return fmt.Errorf("sandbox temporary directory: %w", err)
			}
			if !filepath.IsAbs(args[0]) {
				return fmt.Errorf("sandbox executable must be an absolute path")
			}
			if !inner {
				return enterBubblewrap(root, privateTemp, args)
			}
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
				return fmt.Errorf("enable no_new_privs: %w", err)
			}
			rules := []landlock.Rule{
				landlock.RODirs("/"),
				landlock.RWDirs(root, privateTemp),
			}
			if _, err := os.Stat("/dev/null"); err == nil {
				rules = append(rules, landlock.RWFiles("/dev/null"))
			}
			// V3 is deliberate: it is the first ABI that also restricts file
			// truncation. Strict mode fails closed instead of silently running
			// an unrestricted command on a kernel without the required LSM.
			if err := landlock.V3.RestrictPaths(rules...); err != nil {
				return fmt.Errorf("establish filesystem confinement: %w", err)
			}
			if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
				return fmt.Errorf("execute confined command: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&workspace, "workspace", "", "physical session workspace")
	command.Flags().StringVar(&temp, "temp", "", "private writable temporary directory")
	command.Flags().BoolVar(&inner, "inner", false, "internal sandbox stage")
	_ = command.MarkFlagRequired("workspace")
	_ = command.MarkFlagRequired("temp")
	return command
}

func enterBubblewrap(root, privateTemp string, command []string) error {
	bwrap, err := trustedBubblewrap(privateTemp)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate ALT executable: %w", err)
	}
	arguments := []string{
		"bwrap",
		"--die-with-parent",
		"--new-session",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-net",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run/user",
		"--dir", root,
		"--bind", root, root,
		"--dir", privateTemp,
		"--bind", privateTemp, privateTemp,
		"--dir", filepath.Dir(executable),
		"--ro-bind", executable, executable,
		"--chdir", root,
		"--setenv", "TMPDIR", privateTemp,
		"--setenv", "TEMP", privateTemp,
		"--setenv", "TMP", privateTemp,
		"--",
		executable,
		"__tool-exec",
		"--inner",
		"--workspace", root,
		"--temp", privateTemp,
		"--",
	}
	arguments = append(arguments, command...)
	if err := syscall.Exec(bwrap, arguments, os.Environ()); err != nil {
		return fmt.Errorf("enter Bubblewrap sandbox: %w", err)
	}
	return nil
}

func trustedBubblewrap(privateTemp string) (string, error) {
	for _, candidate := range []string{"/usr/bin/bwrap", "/bin/bwrap"} {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	path, err := installEmbeddedBubblewrap(privateTemp)
	if err != nil {
		return "", fmt.Errorf(
			"safe terminal execution requires Bubblewrap; system lookup and embedded fallback failed: %w",
			err,
		)
	}
	return path, nil
}

func physicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	physical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(physical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", physical)
	}
	return physical, nil
}
