package channel

import (
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strings"
)

// clientOptions is the parsed asb-client command line. A project token
// (asb_p_) selects device authorization; any other token is sent as the
// session bearer exactly as before.
type clientOptions struct {
	endpoint      string
	token         string
	stateDir      string
	project       string
	deviceMode    bool
	nickname      string
	codexSteerOff bool
	toolProfile   string
}

func parseClientOptions(args []string) (clientOptions, error) {
	for _, arg := range args {
		if arg == "--token-file" || arg == "-token-file" || strings.HasPrefix(arg, "--token-file=") || strings.HasPrefix(arg, "-token-file=") {
			return clientOptions{}, errors.New("--token-file is no longer supported; use --token with the token value")
		}
	}
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	endpoint := flags.String("broker-url", "", "project-scoped HTTP or HTTPS broker MCP URL (local or remote)")
	profile := flags.String("tool-profile", "full", "client tool list: full or worker")
	steer := flags.String("codex-steer", "on", "daemon Codex delivery: on or off")
	nickname := flags.String("nickname", "", "requested communication nickname for direct registration; server may assign a fallback")
	token := flags.String("token", "", "project admission token or configured session credential; sent only as an Authorization header")
	stateDir := flags.String("state-dir", "", "directory for this client's durable registration state (default: user config dir)")
	authDir := flags.String("auth-dir", "", "directory for this client's session credentials and registration state; alias of --state-dir")
	if err := flags.Parse(args); err != nil {
		// Flag errors can quote caller input, including a misplaced token.
		return clientOptions{}, errors.New("invalid client options; use --help for supported options")
	}
	if flags.NArg() != 0 {
		return clientOptions{}, errors.New("unexpected positional argument")
	}
	provided := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if !provided["token"] {
		return clientOptions{}, errors.New("--token is required")
	}
	valid := len(*token) > 0 && len(*token) <= 4096
	for _, ch := range *token {
		if ch < 33 || ch > 126 {
			valid = false
			break
		}
	}
	if !valid {
		return clientOptions{}, errors.New("--token requires a nonempty token value of at most 4096 printable ASCII characters without whitespace")
	}
	if err := validateBrokerURL(*endpoint); err != nil {
		return clientOptions{}, err
	}
	options := clientOptions{endpoint: *endpoint, token: *token, deviceMode: isProjectToken(*token)}
	if *steer != "on" && *steer != "off" {
		return clientOptions{}, errors.New("--codex-steer requires on or off")
	}
	if *profile != "full" && *profile != "worker" {
		return clientOptions{}, errors.New("--tool-profile requires full or worker")
	}
	options.toolProfile = *profile
	options.codexSteerOff = *steer == "off"
	options.nickname = *nickname
	if len(options.nickname) > 128 {
		return clientOptions{}, errors.New("--nickname must be at most 128 bytes")
	}
	options.project, _ = projectFromBrokerURL(*endpoint)
	// --auth-dir and --state-dir name one directory. Both flags may be given
	// only when they agree; the default stays with clientstate (environment
	// variable, then the user configuration directory).
	if provided["auth-dir"] && provided["state-dir"] && filepath.Clean(*authDir) != filepath.Clean(*stateDir) {
		return clientOptions{}, errors.New("--auth-dir and --state-dir must name the same directory")
	}
	options.stateDir = *stateDir
	if provided["auth-dir"] {
		options.stateDir = *authDir
	}
	if options.stateDir != "" && !filepath.IsAbs(options.stateDir) {
		return clientOptions{}, errors.New("--auth-dir requires an absolute path")
	}
	return options, nil
}
