package collector

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// aptEntry is one expanded apt source: a single type, URI and suite.
type aptEntry struct {
	file string
	kind string
	// uri is as configured, credentials included; it only names the list
	// files and is sanitized before it is reported.
	uri        string
	suite      string
	components []string
	enabled    bool
	signedBy   *string
	trusted    bool
	arches     []string
}

// aptRepositoryID is "<suite>:<hash>", the hash covering the file, type,
// sanitized URI, suite and components.
func (entry aptEntry) id() string {
	return entry.suite + ":" + shortHash(entry.file, entry.kind, sanitizeURI(entry.uri), entry.suite, strings.Join(entry.components, " "))
}

func (entry aptEntry) repository() Repository {
	return Repository{
		ID:         entry.id(),
		File:       entry.file,
		Type:       entry.kind,
		Enabled:    entry.enabled,
		URIs:       []string{sanitizeURI(entry.uri)},
		Suite:      entry.suite,
		Components: entry.components,
		Signing:    RepositorySigning{SignedBy: entry.signedBy, Trusted: entry.trusted},
		Transport:  uriTransport(entry.uri),
	}
}

// aptEntries reads /etc/apt/sources.list and the *.list and *.sources
// files of sources.list.d, in apt's order.
func (host *Host) aptEntries() []aptEntry {
	var entries []aptEntry
	if data, ok := host.readSourceFile(aptSourcesList); ok {
		entries = append(entries, parseAptList(aptSourcesList, data)...)
	}
	for _, file := range host.sourceFiles(aptSourcesParts, ".list", ".sources") {
		data, ok := host.readSourceFile(file)
		if !ok {
			continue
		}
		if strings.HasSuffix(file, ".sources") {
			entries = append(entries, parseDeb822Sources(file, data)...)
		} else {
			entries = append(entries, parseAptList(file, data)...)
		}
	}
	return entries
}

// parseAptList parses the one-line format:
//
//	deb [ option=value ... ] uri suite [component ...]
//
// Everything after "#" is a comment. Malformed lines are skipped, where
// apt would reject the file.
func parseAptList(file string, data []byte) []aptEntry {
	var entries []aptEntry
	for _, line := range lines(data) {
		if index := strings.IndexByte(line, '#'); index >= 0 {
			line = line[:index]
		}
		words := aptWords(line)
		if len(words) < 3 || (words[0] != RepositoryDeb && words[0] != RepositoryDebSrc) {
			continue
		}
		entry := aptEntry{file: file, kind: words[0], enabled: true}
		rest := words[1:]
		if strings.HasPrefix(rest[0], "[") {
			options := strings.TrimSuffix(strings.TrimPrefix(rest[0], "["), "]")
			for _, option := range strings.Fields(options) {
				key, value, found := strings.Cut(option, "=")
				if !found {
					continue
				}
				switch strings.ToLower(key) {
				case "signed-by":
					if value = strings.TrimSpace(value); value != "" {
						entry.signedBy = &value
					}
				case "trusted":
					entry.trusted = aptBool(value, false)
				case "arch":
					entry.arches = strings.Split(value, ",")
				}
			}
			rest = rest[1:]
		}
		if len(rest) < 2 {
			continue
		}
		entry.uri, entry.suite, entry.components = rest[0], rest[1], rest[2:]
		if !aptValidSuite(entry.suite, entry.components) {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// aptWords splits a one-line source like apt: whitespace separates words,
// except inside "[...]" (the options, cdrom:[label]/) and double quotes,
// which are removed.
func aptWords(line string) []string {
	var words []string
	var word strings.Builder
	depth, quoted, inWord := 0, false, false
	for _, character := range line {
		switch {
		case character == '"':
			quoted, inWord = !quoted, true
			continue
		case character == '[' && !quoted:
			depth++
		case character == ']' && !quoted && depth > 0:
			depth--
		case (character == ' ' || character == '\t') && !quoted && depth == 0:
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
			continue
		}
		word.WriteRune(character)
		inWord = true
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// aptValidSuite: a suite ending in "/" is a flat repository without
// components, any other suite needs at least one.
func aptValidSuite(suite string, components []string) bool {
	if strings.HasSuffix(suite, "/") {
		return len(components) == 0
	}
	return len(components) > 0
}

// aptBool parses apt's boolean spellings, returning fallback for anything
// else.
func aptBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "yes", "true", "with", "on", "enable", "1":
		return true
	case "no", "false", "without", "off", "disable", "0":
		return false
	}
	return fallback
}

// parseDeb822Sources parses a *.sources file. Each stanza is expanded into
// one entry per type, URI and suite; stanzas without types, URIs or
// suites are skipped.
func parseDeb822Sources(file string, data []byte) []aptEntry {
	var entries []aptEntry
	for _, stanza := range parseDeb822(data) {
		types := strings.Fields(stanza["types"])
		uris := strings.Fields(stanza["uris"])
		suites := strings.Fields(stanza["suites"])
		components := strings.Fields(stanza["components"])
		if len(types) == 0 || len(uris) == 0 || len(suites) == 0 {
			continue
		}
		enabled := aptBool(stanza["enabled"], true)
		trusted := aptBool(stanza["trusted"], false)
		signedBy := deb822SignedBy(stanza["signed-by"])
		arches := strings.Fields(stanza["architectures"])
		for _, kind := range types {
			if kind != RepositoryDeb && kind != RepositoryDebSrc {
				continue
			}
			for _, uri := range uris {
				for _, suite := range suites {
					if !aptValidSuite(suite, components) {
						continue
					}
					entries = append(entries, aptEntry{
						file: file, kind: kind, uri: uri, suite: suite, components: components,
						enabled: enabled, signedBy: signedBy, trusted: trusted, arches: arches,
					})
				}
			}
		}
	}
	return entries
}

// parseDeb822 splits deb822 data into stanzas of lower-cased field names.
// Continuation lines are joined with "\n" (" ." is an empty line), lines
// starting with "#" are comments and blank lines separate stanzas.
func parseDeb822(data []byte) []map[string]string {
	var stanzas []map[string]string
	stanza := map[string]string{}
	last := ""
	flush := func() {
		if len(stanza) > 0 {
			stanzas = append(stanzas, stanza)
		}
		stanza, last = map[string]string{}, ""
	}
	for _, line := range lines(data) {
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "#"):
		case line[0] == ' ' || line[0] == '\t':
			if last == "" {
				continue
			}
			continuation := strings.TrimSpace(line)
			if continuation == "." {
				continuation = ""
			}
			stanza[last] += "\n" + continuation
		default:
			key, value, found := strings.Cut(line, ":")
			if !found {
				last = ""
				continue
			}
			last = strings.ToLower(strings.TrimSpace(key))
			stanza[last] = strings.TrimSpace(value)
		}
	}
	flush()
	return stanzas
}

// deb822SignedBy returns a Signed-By value as reported: keyring paths and
// fingerprints as they are, an inline key as the fingerprints of its
// primary keys, or "inline" when they cannot be read. Nil when unset.
func deb822SignedBy(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.Contains(value, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		if fingerprints := armoredFingerprints(value); len(fingerprints) > 0 {
			return stringPointer(strings.Join(fingerprints, ","))
		}
		return stringPointer("inline")
	}
	return stringPointer(strings.Join(strings.Fields(value), " "))
}

// armoredFingerprints returns the upper-case fingerprints of the primary
// keys (v4: SHA-1, v5 and v6: SHA-256) in an ASCII-armored OpenPGP public
// key block, or nil when it cannot be decoded.
func armoredFingerprints(armored string) []string {
	_, body, found := strings.Cut(armored, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	if !found {
		return nil
	}
	body, _, found = strings.Cut(body, "-----END PGP PUBLIC KEY BLOCK-----")
	if !found {
		return nil
	}
	var encoded strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		// Base64 has no ":", so armor headers ("Comment: ...") and blank
		// lines are skipped wherever they are, as is the CRC-24 line.
		if line == "" || strings.Contains(line, ":") || strings.HasPrefix(line, "=") {
			continue
		}
		encoded.WriteString(line)
	}
	packets, err := base64.StdEncoding.DecodeString(encoded.String())
	if err != nil {
		return nil
	}
	var fingerprints []string
	for len(packets) > 0 {
		tag, packet, rest, ok := openPGPPacket(packets)
		if !ok {
			break
		}
		packets = rest
		if tag != 6 || len(packet) == 0 { // 6: public key packet
			continue
		}
		switch packet[0] {
		case 4:
			digest := sha1.New()
			digest.Write([]byte{0x99, byte(len(packet) >> 8), byte(len(packet))})
			digest.Write(packet)
			fingerprints = append(fingerprints, strings.ToUpper(hex.EncodeToString(digest.Sum(nil))))
		case 5, 6:
			digest := sha256.New()
			prefix := byte(0x9a)
			if packet[0] == 6 {
				prefix = 0x9b
			}
			length := make([]byte, 4)
			binary.BigEndian.PutUint32(length, uint32(len(packet)))
			digest.Write([]byte{prefix})
			digest.Write(length)
			digest.Write(packet)
			fingerprints = append(fingerprints, strings.ToUpper(hex.EncodeToString(digest.Sum(nil))))
		}
	}
	return fingerprints
}

// openPGPPacket splits the first packet (old or new format, without
// partial lengths) off data.
func openPGPPacket(data []byte) (tag int, body, rest []byte, ok bool) {
	if len(data) < 2 || data[0]&0x80 == 0 {
		return 0, nil, nil, false
	}
	header := data[0]
	var length, offset int
	if header&0x40 != 0 { // new format
		tag = int(header & 0x3f)
		first := int(data[1])
		switch {
		case first < 192:
			length, offset = first, 2
		case first < 224:
			if len(data) < 3 {
				return 0, nil, nil, false
			}
			length, offset = (first-192)<<8+int(data[2])+192, 3
		case first == 255:
			if len(data) < 6 {
				return 0, nil, nil, false
			}
			length, offset = int(binary.BigEndian.Uint32(data[2:6])), 6
		default:
			return 0, nil, nil, false // partial body lengths do not occur in keys
		}
	} else {
		tag = int(header>>2) & 0x0f
		switch header & 0x03 {
		case 0:
			length, offset = int(data[1]), 2
		case 1:
			if len(data) < 3 {
				return 0, nil, nil, false
			}
			length, offset = int(binary.BigEndian.Uint16(data[1:3])), 3
		case 2:
			if len(data) < 5 {
				return 0, nil, nil, false
			}
			length, offset = int(binary.BigEndian.Uint32(data[1:5])), 5
		default:
			return 0, nil, nil, false
		}
	}
	if length < 0 || offset+length > len(data) {
		return 0, nil, nil, false
	}
	return tag, data[offset : offset+length], data[offset+length:], true
}

// aptListName is apt's URItoFileName: the URI without scheme and
// credentials (IPv6 brackets dropped), with unsafe characters
// percent-encoded in lower case and "/" turned into "_".
func aptListName(uri string) string {
	rest := uri
	if _, after, found := strings.Cut(uri, ":"); found && uriTransport(uri) != "" {
		rest = after
	}
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := strings.IndexByte(rest, '/')
		if end < 0 {
			end = len(rest)
		}
		if at := strings.LastIndexByte(rest[:end], '@'); at >= 0 {
			rest = rest[at+1:]
		}
	}
	if strings.HasPrefix(rest, "[") {
		if end := strings.IndexByte(rest, ']'); end > 0 {
			rest = rest[1:end] + rest[end+1:]
		}
	}
	const unsafe = "\\|{}[]<>\"^~_=!@#$%^&*"
	var name strings.Builder
	for index := 0; index < len(rest); index++ {
		character := rest[index]
		switch {
		case character == '/':
			name.WriteByte('_')
		case character <= 0x20 || character >= 0x7f || strings.IndexByte(unsafe, character) >= 0:
			name.WriteString("%" + hex.EncodeToString([]byte{character}))
		default:
			name.WriteByte(character)
		}
	}
	return name.String()
}

// aptIndexMatcher recognises the Packages list files of an entry: for a
// suite, <uri>_dists_<suite>_<component>_binary-<arch>_Packages per
// component; for a flat repository <uri>_<suite>Packages.
type aptIndexMatcher struct {
	prefixes []string
	exact    string
}

func newAptIndexMatcher(entry aptEntry) aptIndexMatcher {
	base := entry.uri
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	if strings.HasSuffix(entry.suite, "/") {
		suite := entry.suite
		if suite == "/" {
			suite = ""
		}
		return aptIndexMatcher{exact: aptListName(base + suite + "Packages")}
	}
	var matcher aptIndexMatcher
	for _, component := range entry.components {
		matcher.prefixes = append(matcher.prefixes, aptListName(base+"dists/"+entry.suite+"/"+component+"/binary-"))
	}
	return matcher
}

// matches reports whether a list file name, without its compression
// suffix, is one of the entry's Packages indexes.
func (matcher aptIndexMatcher) matches(name string) bool {
	if matcher.exact != "" {
		return name == matcher.exact
	}
	for _, prefix := range matcher.prefixes {
		arch, found := strings.CutPrefix(name, prefix)
		if !found {
			continue
		}
		arch, found = strings.CutSuffix(arch, "_Packages")
		if found && arch != "" && !strings.Contains(arch, "_") {
			return true
		}
	}
	return false
}
