package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/apiop"
)

// Environment variables this CLI reads. Each carries an address or a PATH —
// never a secret. A credential in an environment variable is readable in `ps e`
// output on some systems and is inherited by every child process, which is why
// the token is named by file here and read from it, exactly as
// cmd/waiveo-derive and scripts/devcred already do.
const (
	envAPI      = "WAIVEO_API"
	envTokenFil = "WAIVEO_TOKEN_FILE"
	envCAFile   = "WAIVEO_CA_FILE"
	envInsecure = "WAIVEO_INSECURE_TLS"
	envConfig   = "WAIVEO_CONFIG"
)

// configFileName is the config file's name inside the user's config directory.
const configFileName = "config.json"

// connFlags are the connection flags every subcommand that speaks to a
// deployment shares.
//
// Resolution is FLAG, then ENVIRONMENT, then CONFIG FILE, then nothing. There is
// no built-in default address and no built-in default credential: a CLI that
// silently addressed localhost would, on the box itself, be a CLI that drove
// production because someone forgot an argument.
type connFlags struct {
	api        string
	tokenFile  string
	caFile     string
	insecure   bool
	configPath string
	timeout    time.Duration
}

func (c *connFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.api, "api", "", "deployment base URL, e.g. https://192.168.50.12:7420 ($"+envAPI+", or base_url in the config file)")
	fs.StringVar(&c.tokenFile, "token-file", "", "file holding the api-key bearer token ($"+envTokenFil+", or token_file in the config file)")
	fs.StringVar(&c.caFile, "ca-file", "", "PEM bundle to verify the deployment's certificate against ($"+envCAFile+", or ca_file)")
	fs.BoolVar(&c.insecure, "insecure-tls", false, "skip TLS verification (a dev feeder serves a self-signed ed25519 leaf) ($"+envInsecure+", or insecure_tls)")
	fs.StringVar(&c.configPath, "config", "", "config file (default $"+envConfig+", else "+defaultConfigPathForHelp()+")")
	fs.DurationVar(&c.timeout, "timeout", apiop.DefaultTimeout, "ceiling on one request")
}

// fileConfig is the on-disk config. Unknown members are REFUSED rather than
// ignored: `base_ur1` in a config file is a typo that would otherwise present as
// "the CLI ignores my configuration" with nothing to point at.
type fileConfig struct {
	BaseURL     string `json:"base_url"`
	TokenFile   string `json:"token_file"`
	CAFile      string `json:"ca_file"`
	InsecureTLS *bool  `json:"insecure_tls"`
}

// resolve produces the client configuration, and reports where the address came
// from so a command can print it. Knowing WHICH of three sources supplied the
// address is most of diagnosing "why did that go to the wrong box".
func (c *connFlags) resolve() (apiop.Config, string, error) {
	var file fileConfig
	path, err := c.resolvedConfigPath()
	if err != nil {
		return apiop.Config{}, "", err
	}
	fileUsed := false
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// An absent default config file is the ordinary case. An absent
			// EXPLICITLY-NAMED one is a mistake, and is refused below.
			if c.configPath != "" || os.Getenv(envConfig) != "" {
				return apiop.Config{}, "", fmt.Errorf("config file %s does not exist", path)
			}
		case err != nil:
			return apiop.Config{}, "", fmt.Errorf("read config file %s: %w", path, err)
		default:
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&file); err != nil {
				return apiop.Config{}, "", fmt.Errorf("config file %s: %w", path, err)
			}
			fileUsed = true
		}
	}

	api, source := pick(c.api, "--api", os.Getenv(envAPI), "$"+envAPI, file.BaseURL, "base_url in "+path)
	if api == "" {
		return apiop.Config{}, "", fmt.Errorf("no deployment address: pass --api https://host:7420, set $%s, or put base_url in %s", envAPI, path)
	}
	tokenFile, _ := pick(c.tokenFile, "", os.Getenv(envTokenFil), "", file.TokenFile, "")
	caFile, _ := pick(c.caFile, "", os.Getenv(envCAFile), "", file.CAFile, "")

	insecure := c.insecure
	if !insecure {
		if v := os.Getenv(envInsecure); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return apiop.Config{}, "", fmt.Errorf("$%s=%q is not a boolean", envInsecure, v)
			}
			insecure = b
		} else if fileUsed && file.InsecureTLS != nil {
			insecure = *file.InsecureTLS
		}
	}

	token := ""
	if tokenFile != "" {
		token, err = readTokenFile(expandHome(tokenFile))
		if err != nil {
			return apiop.Config{}, "", err
		}
	} else if def := defaultTokenPath(); def != "" {
		// A default LOCATION is not a default credential: the file is used when it
		// happens to exist and its absence is silent, because the three
		// credential-exchange operations are legitimately called without one.
		if _, statErr := os.Stat(def); statErr == nil {
			token, err = readTokenFile(def)
			if err != nil {
				return apiop.Config{}, "", err
			}
		}
	}

	return apiop.Config{
		BaseURL:     api,
		Token:       token,
		CAFile:      expandHome(caFile),
		InsecureTLS: insecure,
		Timeout:     c.timeout,
	}, source, nil
}

// client resolves the configuration and builds the engine's client over s.
func (c *connFlags) client(s *apiop.Surface) (*apiop.Client, apiop.Config, string, error) {
	cfg, source, err := c.resolve()
	if err != nil {
		return nil, cfg, source, err
	}
	cl, err := apiop.NewClient(s, cfg)
	return cl, cfg, source, err
}

// pick returns the first non-empty of three sources and the name of the one it
// came from.
func pick(flagVal, flagName, envVal, envName, fileVal, fileName string) (string, string) {
	switch {
	case strings.TrimSpace(flagVal) != "":
		return strings.TrimSpace(flagVal), flagName
	case strings.TrimSpace(envVal) != "":
		return strings.TrimSpace(envVal), envName
	case strings.TrimSpace(fileVal) != "":
		return strings.TrimSpace(fileVal), fileName
	}
	return "", ""
}

func (c *connFlags) resolvedConfigPath() (string, error) {
	if c.configPath != "" {
		return expandHome(c.configPath), nil
	}
	if v := strings.TrimSpace(os.Getenv(envConfig)); v != "" {
		return expandHome(v), nil
	}
	dir, err := configDir()
	if err != nil {
		return "", nil // no home directory: run without a config file rather than fail
	}
	return filepath.Join(dir, configFileName), nil
}

func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "waiveo"), nil
}

func defaultConfigPathForHelp() string {
	dir, err := configDir()
	if err != nil {
		return "<user config dir>/waiveo/" + configFileName
	}
	return filepath.Join(dir, configFileName)
}

func defaultTokenPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "token")
}

func expandHome(path string) string {
	if path == "" || !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}

// readTokenFile reads a bearer credential from disk.
//
// It REFUSES a file any group or other bit is set on, and it never renders the
// file's contents in an error. This is scripts/devcred's rule applied to the
// shipped CLI for the same reason: readability is how a bearer credential leaks,
// and silently tightening the mode would hide the exposure rather than end it.
// The refusal says what to do — a key that has been world-readable should be
// revoked and replaced, not chmod-ed.
func readTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("token file %s does not exist", path)
	case err != nil:
		return "", fmt.Errorf("stat token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("token file %s is group- or world-accessible (mode %04o) — revoke that credential and install a fresh one at mode 0600", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("token file %s holds more than one whitespace-separated word — it should hold exactly the token", path)
	}
	return token, nil
}

// readBodyArg turns a --body value into the engine's body arguments.
//
// Three forms, because an operator has three situations: a literal for a
// one-liner, `@path` for something they authored in a file, and `-` for a
// pipeline. An operation whose body is NOT json takes only `@path`: bytes that
// are not text have no literal form, and reading a zip off a terminal is not a
// thing.
func readBodyArg(op apiop.Operation, raw string, stdin io.Reader) (apiop.Args, error) {
	args := apiop.Args{Params: map[string]any{}}
	if raw == "" {
		return args, nil
	}
	if op.Body == nil {
		return args, fmt.Errorf("%s declares no request body", op.ID)
	}
	if !op.Body.JSON {
		if !strings.HasPrefix(raw, "@") {
			return args, fmt.Errorf("%s takes a %s body: pass --body @path-to-file", op.ID, op.Body.MediaType)
		}
		args.BodyPath = expandHome(strings.TrimPrefix(raw, "@"))
		return args, nil
	}
	switch {
	case raw == "-":
		b, err := io.ReadAll(io.LimitReader(stdin, 64<<20))
		if err != nil {
			return args, fmt.Errorf("read body from stdin: %w", err)
		}
		args.Body = b
	case strings.HasPrefix(raw, "@"):
		path := expandHome(strings.TrimPrefix(raw, "@"))
		b, err := os.ReadFile(path)
		if err != nil {
			return args, fmt.Errorf("read --body %s: %w", path, err)
		}
		args.Body = b
	default:
		args.Body = []byte(raw)
	}
	return args, nil
}

// parseWithOperand parses flags around ONE positional argument.
//
// Go's flag package stops parsing at the first non-flag word, so
// `waiveo call listScopeNodes --param limit=3` would otherwise silently treat
// `--param limit=3` as a stray positional and ignore it. That is the shape of
// mistake this CLI must not make: the operator believes they passed a filter,
// the filter never travels, and a list of everything comes back looking like an
// answer. Parsing resumes after the operand so the natural word order works, and
// a SECOND positional is still refused.
func parseWithOperand(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil
	}
	operand := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return "", err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return "", fmt.Errorf("unexpected argument(s) %v — this command takes one operand (%q), and everything else must be a flag", extra, operand)
	}
	return operand, nil
}

// paramList collects repeated `--param k=v` flags.
type paramList map[string]any

func (p paramList) String() string { return "" }

func (p paramList) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	if _, dup := p[k]; dup {
		return fmt.Errorf("%q given twice", k)
	}
	p[k] = val
	return nil
}
