package domain

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
)

// IsLoopbackHost reports whether host names this machine: "localhost" (in any
// case) or a loopback address (127.0.0.0/8, ::1).
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ParseAllowedIPs parses the allowed IP list of the addon settings
// (allowed_ips): comma-separated IP addresses or CIDR subnets, such as
// "192.168.1.0/24, 10.0.0.5". It mirrors the addon's validate_allowed_ips
// (ip_filter.py): the list must not be empty, must have no empty entries
// (leading, trailing or double commas), and every entry must be an address
// or subnet; an address is a single-address prefix and a subnet with host
// bits set is masked. IPv6 zones are refused.
func ParseAllowedIPs(s string) ([]netip.Prefix, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("the allowed IP list must not be empty; for this computer only, use 127.0.0.1")
	}
	var out []netip.Prefix
	var bad []string
	for _, entry := range strings.Split(s, ",") {
		e := strings.TrimSpace(entry)
		if e == "" || strings.ContainsAny(e, " \t") {
			return nil, fmt.Errorf("malformed allowed IP list %q: check for leading or trailing commas, double commas, or missing separators", s)
		}
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil || p.Addr().Zone() != "" {
				bad = append(bad, e)
				continue
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil || a.Zone() != "" {
			bad = append(bad, e)
			continue
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("not an IP address or subnet: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

// LoopbackOnly reports whether every prefix lies within the loopback range,
// so a listener allowing only them binds 127.0.0.1 (the SSH-tunnel setup)
// instead of every interface.
func LoopbackOnly(prefixes []netip.Prefix) bool {
	if len(prefixes) == 0 {
		return false
	}
	for _, p := range prefixes {
		a := p.Addr().Unmap()
		switch {
		case a.Is4() && p.Bits() >= 8 && a.IsLoopback():
		case a.Is6() && p.Bits() == 128 && a.IsLoopback():
		default:
			return false
		}
	}
	return true
}

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

// JoinCommand renders args back into a string SplitCommand parses into the
// same slice, quoting an argument that holds whitespace or a double quote
// (or, off Windows, a single quote, backslash, '$' or backtick, since an
// unquoted one of those also has meaning to SplitCommand there). Inside the
// quotes, '"' is backslash escaped everywhere, and off Windows so are '\',
// '$' and '`': SplitCommand recognises escapes there only off Windows,
// where a literal backslash stays a path separator even inside quotes.
//
// On Windows this one case does not round-trip: SplitCommand there has no
// escape at all for a '"' inside a quoted argument (a literal backslash
// before it stays literal, and the '"' still closes the quote), so an
// argument holding '"' comes back split apart, not with a literal '"'. This
// is accepted rather than fixed: a Windows path cannot contain '"' anyway,
// so it can only affect an extra argument someone typed into
// FREECAD_MCP_FREECAD, never a recorded executable path.
func JoinCommand(args []string) string {
	windows := runtime.GOOS == "windows"
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = quoteCommandArg(a, windows)
	}
	return strings.Join(parts, " ")
}

func quoteCommandArg(s string, windows bool) string {
	needsQuote := s == "" || strings.ContainsAny(s, " \t\n\r\"")
	if !windows {
		needsQuote = needsQuote || strings.ContainsAny(s, "'\\$`")
	}
	if !needsQuote {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || (!windows && strings.ContainsRune("\\$`", r)) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
