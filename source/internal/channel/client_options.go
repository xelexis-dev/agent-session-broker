package channel

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// clientOptions is the parsed asb-client command line. A project token
// (asb_p_) selects device authorization; any other token is sent as the
// session bearer exactly as before.
const (
	envBrokerURL = "AGENT_SESSION_BROKER_URL"
	envToken     = "AGENT_SESSION_BROKER_TOKEN"
	envTokenFile = "AGENT_SESSION_BROKER_TOKEN_FILE"
)

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

// takeEnv reads one input variable and removes it from this process, so no child process or diagnostic sees it.
func takeEnv(name string) string {
	value := os.Getenv(name)
	os.Unsetenv(name)
	return value
}

func parseClientOptions(args []string) (clientOptions, error) {
	// Read and clear every environment input first, even when a flag wins or a value is invalid.
	envURL, envTok, envFile := takeEnv(envBrokerURL), takeEnv(envToken), takeEnv(envTokenFile)
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	endpoint := flags.String("broker-url", "", "project-scoped HTTP or HTTPS broker MCP URL (local or remote)")
	profile := flags.String("tool-profile", "full", "client tool list: full or worker")
	steer := flags.String("codex-steer", "on", "daemon Codex delivery: on or off")
	nickname := flags.String("nickname", "", "requested communication nickname for direct registration; server may assign a fallback")
	token := flags.String("token", "", "project admission token or configured session credential; sent only as an Authorization header")
	tokenFile := flags.String("token-file", "", "absolute path of a private file holding the token")
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
	url := *endpoint
	if !provided["broker-url"] {
		if envURL == "" {
			return clientOptions{}, errors.New("--broker-url or " + envBrokerURL + " is required")
		}
		url = envURL
	}
	// Precedence per value: --token, then the token variable, then --token-file, then the token-file variable.
	secret := *token
	var err error
	switch {
	case provided["token"]:
	case envTok != "":
		secret = envTok
	case provided["token-file"]:
		secret, err = readTokenFile(*tokenFile)
	case envFile != "":
		secret, err = readTokenFile(envFile)
	default:
		return clientOptions{}, errors.New("--token, --token-file, " + envToken + " or " + envTokenFile + " is required")
	}
	if err != nil {
		return clientOptions{}, err
	}
	valid := len(secret) > 0 && len(secret) <= 4096
	for _, ch := range secret {
		if ch < 33 || ch > 126 {
			valid = false
			break
		}
	}
	if !valid {
		return clientOptions{}, errors.New("--token requires a nonempty token value of at most 4096 printable ASCII characters without whitespace")
	}
	if err := validateBrokerURL(url); err != nil {
		return clientOptions{}, err
	}
	options := clientOptions{endpoint: url, token: secret, deviceMode: isProjectToken(secret)}
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
	options.project, _ = projectFromBrokerURL(url)
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

const maxTokenFileBytes = 8 << 10

// readTokenFile reads a token once from a private regular file. Errors name the path at most, never its content;
// a relative path is not echoed because it may be a misplaced token.
func readTokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("token file path must be absolute")
	}
	f, err := openTokenFile(path)
	if err != nil {
		var refused tokenFileRefusal
		if errors.As(err, &refused) {
			return "", fmt.Errorf("token file %s %s", path, string(refused))
		}
		return "", fmt.Errorf("token file %s could not be read", path)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("token file %s could not be read", path)
	}
	if len(data) > maxTokenFileBytes {
		return "", fmt.Errorf("token file %s is larger than 8 KiB", path)
	}
	value := strings.TrimSuffix(string(data), "\n")
	if len(value) < len(data) {
		value = strings.TrimSuffix(value, "\r")
	}
	if value == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return value, nil
}

// tokenFileRefusal is a fixed reason that never contains file content.
type tokenFileRefusal string

func (r tokenFileRefusal) Error() string { return string(r) }

const (
	tokenFileNotRegular = tokenFileRefusal("must be a regular file")
	tokenFileShared     = tokenFileRefusal("must not grant group or other permissions")
	tokenFileForeign    = tokenFileRefusal("must be owned by the current user")
)
