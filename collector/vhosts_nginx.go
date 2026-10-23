package collector

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

const (
	// nginxIncludeDepth bounds nested include expansion.
	nginxIncludeDepth = 8
	// nginxMaxDirectives bounds the expanded configuration.
	nginxMaxDirectives = 500000
)

// nginxDirective is one parsed directive; Block is nil for simple
// directives and non-nil (possibly empty) for blocks.
type nginxDirective struct {
	Name  string
	Args  []string
	File  string
	Line  int
	Block []nginxDirective
}

// nginxDump is the output of nginx -T split into its files, in the order
// nginx printed them (the main configuration file first).
type nginxDump struct {
	order    []string
	sections map[string]string
}

// splitNginxDump splits nginx -T output at its "# configuration file X:"
// markers.
func splitNginxDump(data []byte) nginxDump {
	dump := nginxDump{sections: make(map[string]string)}
	current := ""
	var builder strings.Builder
	flush := func() {
		if current != "" {
			if _, seen := dump.sections[current]; !seen {
				dump.order = append(dump.order, current)
				dump.sections[current] = builder.String()
			}
		}
		builder.Reset()
	}
	for _, line := range lines(data) {
		if name, ok := strings.CutPrefix(line, "# configuration file "); ok && strings.HasSuffix(name, ":") {
			flush()
			current = strings.TrimSuffix(name, ":")
			continue
		}
		if current != "" {
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
	}
	flush()
	return dump
}

type nginxToken struct {
	text   string
	line   int
	quoted bool
}

// tokenizeNginx splits a configuration file into words and the special
// tokens ";", "{" and "}". Comments run from an unquoted "#" at the start
// of a word to the end of the line.
func tokenizeNginx(content string) []nginxToken {
	var tokens []nginxToken
	line := 1
	for index := 0; index < len(content); {
		character := content[index]
		switch {
		case character == '\n':
			line++
			index++
		case character == ' ' || character == '\t' || character == '\r':
			index++
		case character == '#':
			for index < len(content) && content[index] != '\n' {
				index++
			}
		case character == ';' || character == '{' || character == '}':
			tokens = append(tokens, nginxToken{text: string(character), line: line})
			index++
		case character == '"' || character == '\'':
			start := line
			var word strings.Builder
			index++
			for index < len(content) && content[index] != character {
				// Like nginx: \" \' and \\ lose the backslash, \t \r \n
				// are control characters, and other escapes (\. in a
				// regular expression) are kept.
				if content[index] == '\\' && index+1 < len(content) {
					switch next := content[index+1]; next {
					case '"', '\'', '\\':
						index++
					case 't', 'r', 'n':
						word.WriteByte(map[byte]byte{'t': '\t', 'r': '\r', 'n': '\n'}[next])
						index += 2
						continue
					default:
						word.WriteByte('\\')
						index++
					}
				}
				if content[index] == '\n' {
					line++
				}
				word.WriteByte(content[index])
				index++
			}
			index++
			tokens = append(tokens, nginxToken{text: word.String(), line: start, quoted: true})
		default:
			start := index
			for index < len(content) {
				c := content[index]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '{' || c == '}' {
					break
				}
				// "${var}" stays one word.
				if c == '$' && index+1 < len(content) && content[index+1] == '{' {
					if end := strings.IndexByte(content[index:], '}'); end > 0 {
						index += end + 1
						continue
					}
				}
				index++
			}
			tokens = append(tokens, nginxToken{text: content[start:index], line: line})
		}
	}
	return tokens
}

// parseNginxTokens builds the directive tree of one file.
func parseNginxTokens(tokens []nginxToken, file string) []nginxDirective {
	position := 0
	var parse func() []nginxDirective
	parse = func() []nginxDirective {
		directives := []nginxDirective{}
		for position < len(tokens) {
			token := tokens[position]
			if !token.quoted && token.text == "}" {
				position++
				return directives
			}
			if !token.quoted && (token.text == ";" || token.text == "{") {
				position++
				continue
			}
			directive := nginxDirective{Name: token.text, File: file, Line: token.line}
			position++
			for position < len(tokens) {
				next := tokens[position]
				if !next.quoted && next.text == ";" {
					position++
					break
				}
				if !next.quoted && next.text == "{" {
					position++
					directive.Block = parse()
					break
				}
				if !next.quoted && next.text == "}" {
					break
				}
				directive.Args = append(directive.Args, next.text)
				position++
			}
			directives = append(directives, directive)
		}
		return directives
	}
	return parse()
}

// nginxConfig is an nginx -T dump with includes expanded in place.
type nginxConfig struct {
	prefix string
	root   []nginxDirective
}

// expandNginx parses the main file and replaces every include directive
// with the directives of the dumped files its pattern matches. nginx -T
// prints every included file, so nothing is read from disk.
func expandNginx(dump nginxDump) nginxConfig {
	if len(dump.order) == 0 {
		return nginxConfig{}
	}
	main := dump.order[0]
	config := nginxConfig{prefix: path.Dir(main)}
	parsed := make(map[string][]nginxDirective)
	parseFile := func(name string) []nginxDirective {
		if directives, ok := parsed[name]; ok {
			return directives
		}
		directives := parseNginxTokens(tokenizeNginx(dump.sections[name]), name)
		parsed[name] = directives
		return directives
	}
	count := 0
	var expand func(directives []nginxDirective, depth int, stack []string) []nginxDirective
	expand = func(directives []nginxDirective, depth int, stack []string) []nginxDirective {
		result := make([]nginxDirective, 0, len(directives))
		for _, directive := range directives {
			if count++; count > nginxMaxDirectives {
				return result
			}
			if directive.Name == "include" && directive.Block == nil && len(directive.Args) == 1 {
				if depth >= nginxIncludeDepth {
					continue
				}
				pattern := absoluteFrom(config.prefix, directive.Args[0])
				for _, name := range dump.order {
					if matched, _ := path.Match(pattern, name); matched && !slices.Contains(stack, name) {
						result = append(result, expand(parseFile(name), depth+1, append(stack, name))...)
					}
				}
				continue
			}
			if directive.Block != nil {
				directive.Block = expand(directive.Block, depth, stack)
			}
			result = append(result, directive)
		}
		return result
	}
	config.root = expand(parseFile(main), 0, []string{main})
	return config
}

func collectNginx(ctx context.Context, run *vhostRun) ([]VHost, error) {
	host := run.host
	result, err := host.run(ctx, host.findCommand("nginx"), "-T")
	if err != nil {
		return nil, fmt.Errorf("nginx -T: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("nginx -T exited with status %d: %s", result.ExitCode, firstLine(result.Stderr))
	}
	config := expandNginx(splitNginxDump(result.Stdout))
	if config.root == nil {
		return nil, fmt.Errorf("nginx -T printed no configuration")
	}
	return nginxVHosts(config, run), nil
}

// nginxVHosts turns the server blocks of every http block into items.
// Servers whose only names are "_" or "" (catch-all defaults) are skipped.
func nginxVHosts(config nginxConfig, run *vhostRun) []VHost {
	var items []VHost
	for _, http := range config.root {
		if http.Name != "http" || http.Block == nil {
			continue
		}
		inherited := nginxServerDefaults{
			root:        nginxValue(http.Block, "root"),
			certificate: nginxValue(http.Block, "ssl_certificate"),
			upstreams:   make(map[string]string),
		}
		for _, directive := range http.Block {
			if directive.Name == "upstream" && directive.Block != nil && len(directive.Args) == 1 {
				if server := nginxValue(directive.Block, "server"); server != "" {
					inherited.upstreams[directive.Args[0]] = server
				}
			}
		}
		for _, server := range http.Block {
			if server.Name != "server" || server.Block == nil {
				continue
			}
			if item, ok := nginxServer(server, inherited, config.prefix, run); ok {
				items = append(items, item)
			}
		}
	}
	return items
}

type nginxServerDefaults struct {
	root        string
	certificate string
	upstreams   map[string]string
}

func nginxServer(server nginxDirective, defaults nginxServerDefaults, prefix string, run *vhostRun) (VHost, bool) {
	var names []string
	for _, directive := range server.Block {
		if directive.Name == "server_name" {
			for _, name := range directive.Args {
				if name != "_" && name != "" {
					names = append(names, lowerHostname(name))
				}
			}
		}
	}
	if len(names) == 0 {
		return VHost{}, false
	}
	// Prefer a plain name over a regular expression as the primary name.
	primary := slices.IndexFunc(names, func(name string) bool { return !strings.HasPrefix(name, "~") })
	if primary < 0 {
		primary = 0
	}
	item := VHost{
		Name:       names[primary],
		Aliases:    slices.Delete(slices.Clone(names), primary, primary+1),
		ConfigFile: optionalString(server.File),
	}

	listens := false
	for _, directive := range server.Block {
		switch directive.Name {
		case "listen":
			if len(directive.Args) == 0 {
				continue
			}
			port, ok := nginxListenPort(directive.Args[0])
			if !ok {
				continue
			}
			listens = true
			item.Ports = append(item.Ports, port)
			if slices.Contains(directive.Args[1:], "ssl") || slices.Contains(directive.Args[1:], "quic") {
				item.SSL = true
			}
		case "ssl":
			if len(directive.Args) == 1 && directive.Args[0] == "on" {
				item.SSL = true
			}
		}
	}
	if !listens {
		item.Ports = []int{80}
	}

	root := nginxValue(server.Block, "root")
	if root == "" {
		for _, location := range server.Block {
			if location.Name == "location" && location.Block != nil && slices.Equal(location.Args, []string{"/"}) {
				root = nginxValue(location.Block, "root")
			}
		}
	}
	if root == "" {
		root = defaults.root
	}
	if root != "" {
		root = absoluteFrom(prefix, root)
		if trimmed := strings.TrimSuffix(root, "/"); trimmed != "" {
			root = trimmed
		}
		item.DocumentRoot = &root
	}

	if item.SSL {
		certificate := nginxValue(server.Block, "ssl_certificate")
		if certificate == "" {
			certificate = defaults.certificate
		}
		if certificate != "" && !strings.HasPrefix(certificate, "data:") {
			item.Certificate = run.certificate(absoluteFrom(prefix, certificate))
		}
	}

	if pass := nginxFind(server.Block, "fastcgi_pass"); pass != "" {
		if upstream, ok := defaults.upstreams[pass]; ok {
			pass = upstream
		}
		handler := PHPHandlerFPM
		if socket, isSocket := strings.CutPrefix(pass, "unix:"); isSocket && !strings.Contains(socket, "fpm") && strings.Contains(socket, "cgi") {
			handler = PHPHandlerFastCGI
		}
		item.PHP = newPHP(phpVersionFrom(pass), handler)
	}
	return item, true
}

// nginxListenPort returns the port of a listen address: "80", "[::]:443",
// "127.0.0.1:8080" or "*:80". A bare host listens on 80; unix sockets
// have no port.
func nginxListenPort(address string) (int, bool) {
	if strings.HasPrefix(address, "unix:") {
		return 0, false
	}
	portText := address
	if index := strings.LastIndexByte(address, ':'); index >= 0 && !strings.HasSuffix(address, "]") {
		portText = address[index+1:]
	} else if strings.HasPrefix(address, "[") || strings.ContainsAny(address, ".") || address == "localhost" || address == "*" {
		return 80, true
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// nginxValue returns the first argument of the last directive named name
// at this level, which is the one nginx uses for single-valued directives.
func nginxValue(block []nginxDirective, name string) string {
	value := ""
	for _, directive := range block {
		if directive.Name == name && len(directive.Args) > 0 {
			value = directive.Args[0]
		}
	}
	return value
}

// nginxFind returns the first argument of the first directive named name
// at any depth, preferring a unix socket.
func nginxFind(block []nginxDirective, name string) string {
	found := ""
	var walk func([]nginxDirective) bool
	walk = func(directives []nginxDirective) bool {
		for _, directive := range directives {
			if directive.Name == name && len(directive.Args) > 0 {
				if strings.HasPrefix(directive.Args[0], "unix:") {
					found = directive.Args[0]
					return true
				}
				if found == "" {
					found = directive.Args[0]
				}
			}
			if directive.Block != nil && walk(directive.Block) {
				return true
			}
		}
		return false
	}
	walk(block)
	return found
}
