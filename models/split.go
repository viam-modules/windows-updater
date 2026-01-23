package models

import (
	"errors"
	"strings"
)

// SplitExeAndArgs splits a Windows command line into executable (possibly quoted) and the remaining args.
// It handles:
//   - quoted executable paths: "C:\path with spaces\app.exe" /arg1 /arg2
//   - unquoted executable paths containing spaces, by splitting at first .exe
func SplitExeAndArgs(cmd string) (exe string, args string, err error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", "", errors.New("empty command line")
	}

	// Case 1: Executable is quoted: "...\something.exe" ...
	if strings.HasPrefix(cmd, `"`) {
		// Find the closing quote (we don't try to handle escaped quotes inside; Windows paths typically don't include them)
		end := strings.Index(cmd[1:], `"`)
		if end == -1 {
			return "", "", errors.New("unterminated quoted executable")
		}
		end++ // compensate for the [1:] offset

		exe = strings.TrimSpace(cmd[:end+1])  // include closing quote
		args = strings.TrimSpace(cmd[end+1:]) // rest after closing quote
		if exe == `""` {
			return "", "", errors.New("empty quoted executable")
		}
		return exe, args, nil
	}

	// Case 2: Unquoted: split at first .exe (case-insensitive)
	lower := strings.ToLower(cmd)
	exeIdx := strings.Index(lower, ".exe")
	if exeIdx == -1 {
		return "", "", errors.New("no .exe found in command line")
	}

	exeEnd := exeIdx + len(".exe")
	exe = strings.TrimSpace(cmd[:exeEnd])
	args = strings.TrimSpace(cmd[exeEnd:])

	return exe, args, nil
}
