package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
)

// Cursor automatic permission is limited to these installed command paths.
var cursorCommandPaths = [][]string{{"search"}, {"agents", "prefer"}}

// recognizeCursorCommand returns the approved path once both the complete
// command grammar and the hook executable's file identity are established.
func recognizeCursorCommand(command string, executable func() (string, error), paths [][]string) []string {
	argv, ok := simpleCommandArgs(command)
	if !ok || len(argv) < 2 || executable == nil || !filepath.IsAbs(argv[0]) {
		return nil
	}
	own, err := executable()
	if err != nil || !filepath.IsAbs(own) {
		return nil
	}
	own, err = filepath.EvalSymlinks(own)
	if err != nil {
		return nil
	}
	candidate, err := filepath.EvalSymlinks(argv[0])
	if err != nil {
		return nil
	}
	ownInfo, err := os.Stat(own)
	if err != nil || !ownInfo.Mode().IsRegular() {
		return nil
	}
	candidateInfo, err := os.Stat(candidate)
	if err != nil || !os.SameFile(ownInfo, candidateInfo) {
		return nil
	}
	for _, path := range paths {
		if len(argv) > len(path) && slices.Equal(argv[1:1+len(path)], path) {
			return path
		}
	}
	return nil
}

// simpleCommandArgs accepts whitespace-separated words, or whole single- or
// double-quoted words. It deliberately refuses shell expansion, composition,
// redirection, quote concatenation and unfamiliar unquoted punctuation.
// Windows additionally refuses shell-dependent quoting and expansion.
// Single quotes contain literal data; double quotes support only escaped
// backslash and quote. Newlines and controls are refused in every position.
func simpleCommandArgs(command string) ([]string, bool) {
	return simpleCommandArgsForPlatform(command, runtime.GOOS == "windows")
}

func simpleCommandArgsForPlatform(command string, windows bool) ([]string, bool) {
	if windows && strings.ContainsAny(command, "'!%^") {
		return nil, false
	}
	if !utf8.ValidString(command) {
		return nil, false
	}
	for _, r := range command {
		if r < 32 && r != '\t' || r == 127 {
			return nil, false
		}
	}
	var args []string
	for i := 0; i < len(command); {
		if command[i] == ' ' || command[i] == '\t' {
			i++
			continue
		}
		var word strings.Builder
		if command[i] == '\'' || command[i] == '"' {
			quote := command[i]
			i++
			closed := false
			for i < len(command) {
				c := command[i]
				i++
				if c == quote {
					closed = true
					break
				}
				if quote == '"' {
					if c == '$' || c == '`' {
						return nil, false
					}
					if c == '\\' {
						if windows && i < len(command) && command[i] == '"' {
							return nil, false
						}
						if i == len(command) || (command[i] != '\\' && command[i] != '"') {
							return nil, false
						}
						c = command[i]
						i++
					}
				}
				word.WriteByte(c)
			}
			if !closed {
				return nil, false
			}
		} else {
			for i < len(command) && command[i] != ' ' && command[i] != '\t' {
				c := command[i]
				ordinary := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("_./:@%+=,-", rune(c))
				if !ordinary {
					return nil, false
				}
				word.WriteByte(c)
				i++
			}
		}
		if i < len(command) && command[i] != ' ' && command[i] != '\t' {
			return nil, false
		}
		args = append(args, word.String())
	}
	return args, len(args) > 0
}
