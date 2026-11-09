package redact

import (
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// IP address detection for the "ip" PII category.
//
// A bare dotted quad is ambiguous in coding transcripts: version numbers
// (1.17.2.3), section numbers and user agents (Chrome/154.0.0.0) have the same
// shape. Measured on real agent transcripts, matching every valid address
// redacted about one version number for every three real addresses. So a match
// is redacted only when it is shaped like an address in use, the approach
// Presidio and Cloud DLP take with context words:
//
//   - a private, link-local, or shared (CGNAT) address — internal network
//     layout (a version label before it, as in Version=10.1.2.3, still
//     rules it out); or
//   - network syntax around it: scheme (://), userinfo (@), port (:443),
//     CIDR prefix (/24), or a connection arrow (->); or
//   - a network word nearby (ip, host, server, ssh, tcp, dns, ...).
//
// A public address printed bare in a list with no such context is not
// redacted; custom_patterns covers teams that need every address.
// testdata/ip_lines.txt measures the rule's precision and recall.
var (
	// ipv4CandidateRegex has no leading \b, which would never match after the
	// letter of an escape left in the text (\n10.0.0.5); ipStandsAlone rejects
	// a candidate that continues a word on the left instead.
	ipv4CandidateRegex = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}\b`)
	// ipv6CandidateRegex needs at least two colons, so a single key:value
	// colon never becomes a candidate. The optional dotted tail admits
	// IPv4-embedded forms (::ffff:10.0.0.1). netip.ParseAddr does the actual
	// validation.
	ipv6CandidateRegex = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,8}(?:\.\d{1,3}){0,3}`)

	ipContextWordRegex = regexp.MustCompile(`(?i)\b(?:ips?|ipv[46]|host(?:name|s)?|addr(?:ess(?:es)?)?|servers?|tcp|udp|ssh|inet6?|dns|nat|cidr|gateway|subnet|proxy|connect(?:ion|ed|ing|x)?|dial|ping|curl|nc|peer|remote|endpoint|resolve[sd]?|listen(?:ing)?|bind)\b`)
)

// ipContextBefore and ipContextAfter bound the window searched for a
// network word.
const (
	ipContextBefore = 60
	ipContextAfter  = 30
)

// cgnatPrefix is the shared address space (RFC 6598) carriers and overlay
// networks such as Tailscale use; netip does not classify it as private.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// redactableIP reports whether the candidate s[start:end] is an IP address
// that should be redacted, and its bounds after trimming separator colons.
// It requires the context described above.
func redactableIP(s string, start, end int) (int, int, bool) {
	start, end, addr, ok := ipCandidate(s, start, end)
	if !ok {
		return 0, 0, false
	}
	return start, end, ipHasContext(s, start, end, addr)
}

// ipCandidate validates the regex candidate s[start:end] as an address worth
// redacting, without the context rule: it parses the address, rejects
// candidates that are part of a longer token or the value of a version label,
// and skips addresses that identify nothing (loopback, unspecified,
// 0.0.0.0/8, broadcast, N.0.0.0, documentation ranges, and the first address
// of each private block).
func ipCandidate(s string, start, end int) (int, int, netip.Addr, bool) {
	// The IPv6 candidate regex also takes a separator colon on either side
	// ("host:fe80::1", "fe80::1: unreachable"); validate the address between
	// them. A leading or trailing "::" is part of the address.
	if end-start > 2 && s[start] == ':' && s[start+1] != ':' {
		start++
	}
	start, end = trimGluedWords(s, start, end)
	addr, err := netip.ParseAddr(s[start:end])
	if err != nil && end-start > 2 && s[end-1] == ':' && s[end-2] != ':' {
		end--
		addr, err = netip.ParseAddr(s[start:end])
	}
	// A full eight-group address with a port and no brackets reads as nine
	// groups (2600:...:7f80:443); the last one is the port.
	if err != nil && strings.Count(s[start:end], ":") == 8 && !strings.Contains(s[start:end], "::") {
		if i := strings.LastIndexByte(s[start:end], ':'); isPort(s[start+i+1 : end]) {
			end = start + i
			addr, err = netip.ParseAddr(s[start:end])
		}
	}
	if err != nil {
		return 0, 0, netip.Addr{}, false
	}
	addr = addr.Unmap()
	if !ipStandsAlone(s, start, end, addr) {
		return 0, 0, netip.Addr{}, false
	}
	// A hex-only label glued on by a colon (db:fd12::5) makes the whole run
	// parse as one public address; the internal address after it is the one
	// in use.
	if addr.Is6() && !isInternalIP(addr) {
		if i := strings.IndexByte(s[start:end], ':'); i > 0 && start+i+1 < end && s[start+i+1] != ':' {
			if inner, err := netip.ParseAddr(s[start+i+1 : end]); err == nil && isInternalIP(inner.Unmap()) {
				start, addr = start+i+1, inner.Unmap()
			}
		}
	}
	if addr.IsLoopback() || addr.IsUnspecified() {
		return 0, 0, netip.Addr{}, false
	}
	if addr.Is4() {
		b := addr.As4()
		// N.0.0.0 is a /8 network address, never a host; written bare it is
		// a major version, such as .NET's Version=10.0.0.0.
		if b[0] == 0 || b == [4]byte{255, 255, 255, 255} || b[1]|b[2]|b[3] == 0 {
			return 0, 0, netip.Addr{}, false
		}
	}
	if isExampleOrBlockStart(addr) || followsVersionLabel(s, start) {
		return 0, 0, netip.Addr{}, false
	}
	return start, end, addr, true
}

// ipDocumentationPrefixes are reserved for examples (RFC 5737, RFC 3849,
// RFC 9637), so no network uses them.
var ipDocumentationPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("3fff::/20"),
}

// ipBlockStarts are the first addresses of the private, shared, link-local
// and unique local blocks. Code and docs that handle those ranges write them
// (10.0.0.0/8, fe80::/10), and they name a category, not a network.
var ipBlockStarts = map[netip.Addr]bool{
	netip.MustParseAddr("10.0.0.0"):    true,
	netip.MustParseAddr("172.16.0.0"):  true,
	netip.MustParseAddr("192.168.0.0"): true,
	netip.MustParseAddr("100.64.0.0"):  true,
	netip.MustParseAddr("169.254.0.0"): true,
	netip.MustParseAddr("fc00::"):      true,
	netip.MustParseAddr("fd00::"):      true,
	netip.MustParseAddr("fe80::"):      true,
}

func isExampleOrBlockStart(addr netip.Addr) bool {
	if ipBlockStarts[addr] {
		return true
	}
	for _, p := range ipDocumentationPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// followsVersionLabel reports whether the candidate at s[start] is the value
// of a version label (Version=10.0.0.0, AssemblyVersion("10.1.2.3"),
// "version": "10.1.2.3"). A private range does not make such a value an
// address, so this check runs before the context rule.
func followsVersionLabel(s string, start int) bool {
	const label = "version"
	i := start
	for i > 0 && start-i < 4 && strings.IndexByte(" \t=:\"'(", s[i-1]) >= 0 {
		i--
	}
	w := i - len(label)
	if w < 0 || !strings.EqualFold(s[w:i], label) {
		return false
	}
	// A whole word (version, api_version) or a camelCase suffix
	// (AssemblyVersion), but not the tail of conversion or subversion.
	return w == 0 || !isLetterByte(s[w-1]) || s[w] == 'V'
}

// ipStandsAlone rejects a candidate that continues a longer token on either
// side: an identifier or version (go1.2.3.4, v1.2.3.4, 1.2.3.4-rc1), a longer
// dotted number (an OID such as 1.3.6.1.4.1), a user agent (Chrome/154.0.0.0),
// or a code path (u64::from, a[1::2]).
func ipStandsAlone(s string, start, end int, addr netip.Addr) bool {
	wildcardDNS := addr.Is4() && underWildcardDNS(s, end)
	if start > 0 {
		prev := s[start-1]
		if (isIPWordByte(prev) && !followsEscape(s, start)) || (prev == '.' && !wildcardDNS) {
			return false
		}
		// In raw JSON a letter after a backslash belongs to an escape (\f, \b),
		// so the candidate starts mid-token. A digit does not: \\10.0.0.5\share
		// is a Windows share path.
		if prev == '\\' && isLetterByte(s[start]) {
			return false
		}
		if prev == ':' && addr.Is6() && !isSeparatorColon(s, start-1) {
			return false
		}
		if prev == '/' && start > 1 && isIPWordByte(s[start-2]) {
			return false
		}
	}
	if end < len(s) {
		next := s[end]
		if isIPWordByte(next) {
			return false
		}
		// A longer colon run, such as a key fingerprint (MD5:12:f8:...), is
		// not an address even when eight of its groups parse as one. A port
		// (:443) ends the run.
		if next == ':' && addr.Is6() && isHexGroup(wordAt(s, end+1)) && !endsWithPort(s, end+1) {
			return false
		}
		if next == '.' && end+1 < len(s) && isIPWordByte(s[end+1]) && !wildcardDNS {
			return false
		}
		// A dash suffix continues a version (1.2.3.4-rc1, 10.1.2.3-1ubuntu1);
		// the next address of a range (10.0.0.1-10.0.0.9) does not.
		if next == '-' && end+1 < len(s) && isIPWordByte(s[end+1]) && !startsWithIPv4(s[end+1:]) {
			return false
		}
	}
	if addr.Is4() {
		return true
	}
	// IPv6: hex letters joined by colons are also code (c::d, Self::A), so an
	// address needs a digit unless it is private, and a short compressed form
	// (fe80::1, also a[1::2]) needs a clean left edge.
	candidate := s[start:end]
	if !strings.ContainsAny(candidate, "0123456789") && !isInternalIP(addr) {
		return false
	}
	if ipv6Groups(candidate) >= 3 || start == 0 || followsEscape(s, start) {
		return true
	}
	switch s[start-1] {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', '=', '(', ',', '@':
		return true
	case ':':
		return isSeparatorColon(s, start-1)
	case '[':
		// A bracketed host ([fd00::5]:8080), not an index or slice (a[1::2]).
		if start == 1 {
			return true
		}
		before := s[start-2]
		return !isIPWordByte(before) && before != ']' && before != ')'
	}
	return false
}

// wildcardDNSSuffixes are services whose host names embed an address:
// 10.0.0.5.nip.io resolves to 10.0.0.5.
var wildcardDNSSuffixes = []string{".nip.io", ".sslip.io"}

// underWildcardDNS reports whether s[end:] starts with a wildcard DNS suffix
// that ends the host name.
func underWildcardDNS(s string, end int) bool {
	rest := s[end:]
	for _, suffix := range wildcardDNSSuffixes {
		if len(rest) < len(suffix) || !strings.EqualFold(rest[:len(suffix)], suffix) {
			continue
		}
		if len(rest) == len(suffix) || (!isIPWordByte(rest[len(suffix)]) && rest[len(suffix)] != '-') {
			return true
		}
	}
	return false
}

// startsWithIPv4 reports whether t starts with a dotted-quad address. An
// address is at most 15 bytes, so the regex only needs one byte past it.
func startsWithIPv4(t string) bool {
	t = t[:min(len(t), 16)]
	loc := ipv4CandidateRegex.FindStringIndex(t)
	if loc == nil || loc[0] != 0 {
		return false
	}
	_, err := netip.ParseAddr(t[:loc[1]])
	return err == nil
}

// followsEscape reports whether the letter before s[start] ends an escape
// rather than a word: a JSON escape (\n, \t) left in a string decoded only
// once, such as Codex function_call.arguments, or an ANSI color code
// (ESC[35m, \033[35m) from terminal output or a script.
func followsEscape(s string, start int) bool {
	if start < 2 || !isLetterByte(s[start-1]) {
		return false
	}
	if skipJSONEscapeLetter(s, start-1) == start {
		return true
	}
	i := start - 2
	for i >= 0 && start-i < 16 && (isDigit(s[i]) || s[i] == ';') {
		i--
	}
	if i < 0 || s[i] != '[' {
		return false
	}
	for _, esc := range ansiEscapeSpellings {
		if strings.HasSuffix(s[:i], esc) {
			return true
		}
	}
	return false
}

// ansiEscapeSpellings are the ways ESC appears before a color code: raw, as
// a JSON escape, and as written in shell and source strings.
var ansiEscapeSpellings = []string{"\x1b", `\u001b`, `\033`, `\x1b`, `\e`}

// isSeparatorColon reports whether the colon at s[i] ends a word rather than
// a hex group ("host:fe80::1" and "iface:fe80::1", not "ab:fe80::1").
func isSeparatorColon(s string, i int) bool {
	if i == 0 || s[i-1] == ':' {
		return false
	}
	j := i
	for j > 0 && i-j <= 4 && isIPWordByte(s[j-1]) {
		j--
	}
	return !isHexGroup(s[j:i])
}

// trimGluedWords drops a group the candidate s[start:end] took from a word
// glued to the address by a colon. The regex reads the hex letters at the end
// of "iface:" or the start of ":could" as a group ("face:fe80::1",
// "fe80::1:c"), and the whole candidate would then be rejected as part of
// that word. A group joined by "::" is kept, so code paths (u64::from) stay
// whole and are rejected.
func trimGluedWords(s string, start, end int) (int, int) {
	if start > 0 && isIPWordByte(s[start-1]) && !followsEscape(s, start) {
		if i := strings.IndexByte(s[start:end], ':'); i > 0 && start+i+1 < end && s[start+i+1] != ':' && isSeparatorColon(s, start+i) {
			start += i + 1
		}
	}
	if end < len(s) && isIPWordByte(s[end]) {
		if i := strings.LastIndexByte(s[start:end], ':'); i > 0 && s[start+i-1] != ':' && !isHexGroup(wordAt(s, start+i+1)) {
			end = start + i
		}
	}
	return start, end
}

// isPort reports whether w is a decimal port number.
func isPort(w string) bool {
	if w == "" || len(w) > 5 {
		return false
	}
	for i := range len(w) {
		if !isDigit(w[i]) {
			return false
		}
	}
	return true
}

// endsWithPort reports whether s[i:] starts with a port that is not followed
// by another colon group.
func endsWithPort(s string, i int) bool {
	w := wordAt(s, i)
	return isPort(w) && (i+len(w) == len(s) || s[i+len(w)] != ':')
}

// wordAt returns up to five word bytes of s starting at i, enough to tell a
// hex group from a longer word.
func wordAt(s string, i int) string {
	j := i
	for j < len(s) && j-i < 5 && isIPWordByte(s[j]) {
		j++
	}
	return s[i:j]
}

// isHexGroup reports whether w could be one IPv6 group: one to four hex digits.
func isHexGroup(w string) bool {
	if w == "" || len(w) > 4 {
		return false
	}
	for i := range len(w) {
		if !isHexByte(w[i]) {
			return false
		}
	}
	return true
}

// isInternalIP reports whether addr is private, link-local, or shared
// (CGNAT) address space.
func isInternalIP(addr netip.Addr) bool {
	return addr.IsPrivate() || addr.IsLinkLocalUnicast() || cgnatPrefix.Contains(addr)
}

// ipv6Groups counts the non-empty colon-separated groups of an IPv6 candidate.
func ipv6Groups(candidate string) int {
	n := 0
	for _, g := range strings.Split(candidate, ":") {
		if g != "" {
			n++
		}
	}
	return n
}

// ipHasContext reports whether the address is shaped like one in use; the
// rule is described at the top of this file.
func ipHasContext(s string, start, end int, addr netip.Addr) bool {
	if isInternalIP(addr) {
		return true
	}
	before, after := s[:start], s[end:]
	if strings.HasSuffix(before, "://") || strings.HasSuffix(before, "//[") ||
		strings.HasSuffix(before, "@") || strings.HasSuffix(before, "->") ||
		strings.HasPrefix(after, "->") || strings.HasPrefix(after, "]:") {
		return true
	}
	if len(after) >= 2 && (after[0] == ':' || after[0] == '/') && isDigit(after[1]) {
		return true
	}
	before, after = ipContextWindow(s, start, end)
	return ipContextWordRegex.MatchString(before) || ipContextWordRegex.MatchString(after)
}

// ipContextWindow returns the text searched for a network word on each side
// of s[start:end]. It stays on the address's own line, because one transcript
// string holds a whole command output, and it drops a word the window cuts,
// because \b would match at the cut ("ghost" seen as "host").
func ipContextWindow(s string, start, end int) (string, string) {
	lo := max(start-ipContextBefore, 0)
	if i := strings.LastIndexByte(s[lo:start], '\n'); i >= 0 {
		lo += i + 1
	}
	for lo > 0 && lo < start && isIPWordByte(s[lo-1]) {
		lo++
	}
	hi := min(end+ipContextAfter, len(s))
	if i := strings.IndexByte(s[end:hi], '\n'); i >= 0 {
		hi = end + i
	}
	for hi < len(s) && hi > end && isIPWordByte(s[hi]) {
		hi--
	}
	return separateEscapes(s[lo:start]), separateEscapes(s[end:hi])
}

// windowEscapeRegex matches escapes that glue onto the next word: a JSON
// escape left in text decoded only once (\nssh) and an ANSI color code
// (ESC[1mssh), written raw or as an escape. A literal \n is not taken as a
// line break, because a Windows path (C:\keys\node1.pem) or a C string has
// the same bytes.
var windowEscapeRegex = regexp.MustCompile(`\\[nrt]|(?:\x1b|\\u001b|\\033|\\x1b|\\e)\[[0-9;]*[A-Za-z]`)

// separateEscapes replaces the escapes in a context window with spaces so
// the word after each one starts at a word boundary.
func separateEscapes(w string) string {
	if !strings.ContainsAny(w, "\\\x1b") {
		return w
	}
	return windowEscapeRegex.ReplaceAllString(w, " ")
}

// mayContainIPv4 reports whether s has a digit-dot-digit sequence, which
// every dotted quad contains.
func mayContainIPv4(s string) bool {
	for i := 1; i+1 < len(s); i++ {
		if s[i] == '.' && isDigit(s[i-1]) && isDigit(s[i+1]) {
			return true
		}
	}
	return false
}

// mayContainIPv6 reports whether s has "::" or a hex group between two
// colons (":ab:"), one of which every IPv6 address with three or more groups
// or a compressed form contains.
func mayContainIPv6(s string) bool {
	for i := strings.IndexByte(s, ':'); i >= 0 && i+1 < len(s); {
		j := i + 1
		for j < len(s) && j-i <= 5 && isHexByte(s[j]) {
			j++
		}
		if j < len(s) && s[j] == ':' {
			return true
		}
		next := strings.IndexByte(s[j:], ':')
		if next < 0 {
			return false
		}
		i = j + next
	}
	return false
}

func isHexByte(c byte) bool {
	return isDigit(c) || (c|0x20 >= 'a' && c|0x20 <= 'f')
}

// redactIPsUnderNetworkKey redacts the addresses in a JSON string value whose
// key names one ({"host":"34.117.59.81"}, {"remote_addr":...}). The JSONL path
// scans each value without its key, so the key is the only context such a
// public address has. It runs on the already-redacted value and counts its
// matches in t, like the PII layer.
func redactIPsUnderNetworkKey(key, val string, t *tally) string {
	if !ipCategoryEnabled() || !isNetworkKey(key) {
		return val
	}
	var regions []taggedRegion
	for _, re := range []*regexp.Regexp{ipv4CandidateRegex, ipv6CandidateRegex} {
		for _, loc := range re.FindAllStringIndex(val, -1) {
			if start, end, _, ok := ipCandidate(val, loc[0], loc[1]); ok {
				regions = append(regions, taggedRegion{region: region{start, end}, label: labelIP, layer: layerPII, builtinIP: true})
			}
		}
	}
	regions = dropNestedIPRegions(regions)
	// The key names the value, so it is context only for addresses the value
	// holds directly (a list, a URL, user@host), not for one inside
	// descriptive text ({"hostname":"build 1.17.2.3"}).
	kept := regions[:0]
	from := 0
	for _, r := range regions {
		if !hasWordsBefore(val[from:r.start]) {
			kept = append(kept, r)
		}
		from = r.end
	}
	return redactRegions(val, kept, t)
}

// placeholderRegex matches the tokens redaction writes, which are not
// descriptive text.
var placeholderRegex = regexp.MustCompile(`\[?REDACTED(?:_[A-Z]+)?\]?`)

// hasWordsBefore reports whether the text between one address and the next
// holds descriptive words. A URL scheme, userinfo, redaction placeholders and
// escapes are not words, and a label that ends in : or = (HOST_IP=,
// "server": ") names the address that follows.
func hasWordsBefore(seg string) bool {
	if i := strings.LastIndex(seg, "://"); i >= 0 {
		seg = seg[i+len("://"):]
	}
	if i := strings.LastIndexByte(seg, '@'); i >= 0 {
		seg = seg[i+1:]
	}
	seg = separateEscapes(placeholderRegex.ReplaceAllString(seg, ""))
	if label := strings.TrimRight(seg, " \t\"'[(\\"); strings.HasSuffix(label, ":") || strings.HasSuffix(label, "=") {
		return false
	}
	for i := range len(seg) {
		if isLetterByte(seg[i]) {
			return true
		}
	}
	return false
}

// ipCategoryEnabled reports whether the built-in IP rules run.
func ipCategoryEnabled() bool {
	cfg := getPIIConfig()
	return cfg != nil && cfg.Enabled && cfg.Categories[PIIIP]
}

// isVersionValue reports whether val is the value of a version key (version,
// assemblyVersion, api_version) and is nothing but an address-shaped
// version ({"version":"10.1.2.3"}). The JSONL walk scans a value without its
// key, so this is the version-label rule for JSON.
func isVersionValue(key, val string) bool {
	v := strings.TrimSpace(val)
	if len(v) > 64 || !ipCategoryEnabled() {
		return false
	}
	if _, err := netip.ParseAddr(v); err != nil {
		return false
	}
	k := normalizeCredentialJSONKey(key)
	return k == "version" || strings.HasSuffix(k, "_version")
}

// dropNestedIPRegions removes an IP region that lies inside another one, so
// an address counts once: both IP rules match an IPv4-mapped address, the
// IPv4 rule only its dotted tail (::ffff:10.0.0.1).
func dropNestedIPRegions(regions []taggedRegion) []taggedRegion {
	if len(regions) < 2 {
		return regions
	}
	slices.SortFunc(regions, func(a, b taggedRegion) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	kept := regions[:1]
	for _, r := range regions[1:] {
		last := kept[len(kept)-1]
		if r.label == labelIP && last.label == labelIP && r.end <= last.end {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// isNetworkKey reports whether a JSON key names an address. The key is split
// into words first (HOST_IP, clientIP and IPAddress become host ip, client
// ip and ip address), since \b does not separate snake_case or camelCase.
// camelCase splitting would cut the IP acronym before a lowercase letter
// (IPs: i_ps, IPv6: i_pv6), so those are marked as words first, and a key
// with a digit is also read broken after each digit (IPv4Address: ipv4
// address).
func isNetworkKey(key string) bool {
	if ipContextWordRegex.MatchString(strings.ReplaceAll(normalizeCredentialJSONKey(ipAcronymWords.Replace(key)), "_", " ")) {
		return true
	}
	return strings.ContainsAny(key, "0123456789") && ipContextWordRegex.MatchString(splitAfterDigits(key))
}

var ipAcronymWords = strings.NewReplacer("IPs", "_ips_", "IPv", "_ipv")

func splitAfterDigits(key string) string {
	var b strings.Builder
	for i := range len(key) {
		c := key[i]
		switch {
		case isLetterByte(c):
			if i > 0 && isDigit(key[i-1]) {
				b.WriteByte(' ')
			}
			b.WriteByte(c | 0x20)
		case isDigit(c):
			b.WriteByte(c)
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func isIPWordByte(c byte) bool {
	return isASCIIAlnum(c) || c == '_'
}

func isLetterByte(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
