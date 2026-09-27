package domain

import (
	"fmt"
	"net"
	"runtime"
	"strings"
)

// ValidateHost accepts an IPv4 or IPv6 address or an RFC 1123 host name.
func ValidateHost(host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > 253 {
		return fmt.Errorf("invalid host %q: must be an IP address or host name", host)
	}
	for _, label := range strings.Split(name, ".") {
		if !validLabel(label) {
			return fmt.Errorf("invalid host %q: must be an IP address or host name", host)
		}
	}
	return nil
}

func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// SplitCommand splits a command line into arguments the way a POSIX shell
// (and Python's shlex.split) does: whitespace separates words, single quotes
// are literal, and double quotes group words. Backslash escapes the next
// character outside quotes, and inside double quotes only before ", \, $ and
// `. On Windows, backslash is a path separator and an apostrophe a letter
// (C:\Users\O'Neil), so both stay literal and only double quotes group.
func SplitCommand(s string) ([]string, error) {
	windows := runtime.GOOS == "windows"
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && !windows && i+1 < len(runes) && strings.ContainsRune("\"\\$`", runes[i+1]):
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\' && !windows:
			escaped, inWord = true, true
		case r == '"' || r == '\'' && !windows:
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in %q", quote, s)
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash in %q", s)
	}
	if inWord {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return args, nil
}
