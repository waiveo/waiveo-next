package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The Roku dev credential is NOT in this repo and never will be. It lives in
// the operator's own dev-lab env file (default ~/.config/waiveo/dev-lab.env,
// mode 0600, itself restored from the vault), which is a plain list of shell
// `export KEY=value` lines — the exact file `waiveo/scripts/cli/lib/load-env.sh`
// cats for the legacy CLIs, read here directly so this tool works whether or
// not that shell helper has been eval'd into the caller's environment.
//
// Precedence is process environment FIRST, file second: a caller who already
// ran `eval "$(… load-env.sh)"`, or who is overriding one value for one run,
// must not have the file quietly win over what they just set.

// envFileVar names the override the legacy helper honours, so pointing both at
// the same non-default file takes one variable, not two conventions.
const envFileVar = "WAIVEO_ENV_FILE"

// defaultRokuUser is the Roku developer-mode account name. It is fixed by the
// platform (the dev web installer has exactly one user), so it is a default
// rather than a required input; only the password is a secret.
const defaultRokuUser = "rokudev"

// credentials is one resolved dev-installer login. Password is never printed,
// logged, or included in an error message anywhere in this program.
type credentials struct {
	User     string
	Password string
}

// defaultEnvFilePath is the dev-lab env file this tool reads when the caller
// names none: $WAIVEO_ENV_FILE, else ~/.config/waiveo/dev-lab.env. An
// unresolvable home directory yields "", which loadEnvFile reads as "no file",
// leaving the process environment as the only source.
func defaultEnvFilePath(getenv func(string) string) string {
	if p := getenv(envFileVar); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "waiveo", "dev-lab.env")
}

// loadEnvFile parses a shell env file into a map, tolerating the three shapes
// the real file uses: `export KEY=value`, `KEY=value`, and quoted values. A
// missing file is NOT an error — the process environment may already carry
// everything needed — but an unreadable one is, because silently falling back
// from a file the caller explicitly named would produce a confusing "no
// password configured" instead of "cannot read that file".
//
// It is deliberately not a shell: no interpolation, no command substitution,
// no line continuation. Anything fancier than a literal assignment is a
// credential file that should be simplified, not a parser this tool should
// grow.
func loadEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	return values, nil
}

// unquote strips one matched pair of surrounding single or double quotes —
// the only quoting the env file uses, and the only quoting this parser claims
// to understand.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// resolveCredentials picks the dev-installer login from the process
// environment first and the env file second (see this file's own precedence
// note). userOverride, when non-empty, beats both — that is what a -user flag
// is for.
//
// A missing password is a hard error naming the file and the vault item,
// rather than a default or a prompt: this tool runs unattended against a wall
// of screens, and the failure has exactly one remedy.
func resolveCredentials(getenv func(string) string, fileValues map[string]string, userOverride, envFilePath string) (credentials, error) {
	pick := func(key string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fileValues[key]
	}

	user := userOverride
	if user == "" {
		user = pick("ROKU_DEV_USER")
	}
	if user == "" {
		user = defaultRokuUser
	}

	password := pick("ROKU_DEV_PASSWORD")
	if password == "" {
		where := envFilePath
		if where == "" {
			where = "the dev-lab env file"
		}
		return credentials{}, fmt.Errorf(
			"no Roku dev password: set ROKU_DEV_PASSWORD in the environment or in %s "+
				"(restore that file from the vault item \"Waiveo Dev Lab Environment\")", where)
	}
	return credentials{User: user, Password: password}, nil
}
