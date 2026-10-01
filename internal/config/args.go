package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// ParseServe builds the server configuration from the environment and the
// serve command line. Flags take precedence over their environment
// variables and are applied before validation, so the combined result is
// checked as a whole.
//
// --cmd (also -cmd and -c) and -p/--port keep the legacy CLI's meaning.
// --cmd names the terminal executable exactly as given; arguments after
// "--" are passed to it verbatim and never through a shell. The returned
// warnings describe accepted legacy flags that no longer have an effect.
func ParseServe(lookup func(string) (string, bool), args []string, output io.Writer) (Config, []string, error) {
	flags := flag.NewFlagSet("webpty", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: webpty [serve] [flags] [-- command arguments]")
		fmt.Fprintln(output, "\nFlags override the WEBPTY_* environment variable named in each description.")
		flags.PrintDefaults()
	}

	var address, port, database, origin, command, allowedHosts, password, keepalive string
	var secure bool
	flags.StringVar(&address, "address", "", "listen `host:port` (WEBPTY_ADDRESS, default 127.0.0.1:8000)")
	portUsage := "listen `port`, keeping the configured host (legacy; conflicts with --address)"
	flags.StringVar(&port, "port", "", portUsage)
	flags.StringVar(&port, "p", "", portUsage)
	flags.StringVar(&database, "database", "", "SQLite database `path` (WEBPTY_DATABASE_PATH, default webpty.db)")
	flags.StringVar(&origin, "public-origin", "", "`URL` browsers use, required off loopback (WEBPTY_PUBLIC_ORIGIN)")
	flags.BoolVar(&secure, "secure-cookies", false, "mark cookies Secure; defaults on for an https public origin (WEBPTY_SECURE_COOKIES)")
	commandUsage := "terminal `executable`, run without a shell; its arguments follow -- (WEBPTY_COMMAND, default $SHELL)"
	flags.StringVar(&command, "cmd", "", commandUsage)
	flags.StringVar(&command, "c", "", commandUsage)
	hostsUsage := "deprecated and ignored: use --public-origin"
	flags.StringVar(&allowedHosts, "allowed-hosts", "", hostsUsage)
	flags.StringVar(&allowedHosts, "ah", "", hostsUsage)
	passwordUsage := "no longer supported: the administrator sets a password at first sign-in"
	flags.StringVar(&password, "password", "", passwordUsage)
	flags.StringVar(&password, "pass", "", passwordUsage)
	keepaliveUsage := "deprecated and ignored: use WEBPTY_WS_PING_INTERVAL"
	flags.StringVar(&keepalive, "keepalive", "", keepaliveUsage)
	flags.StringVar(&keepalive, "k", "", keepaliveUsage)
	if err := flags.Parse(args); err != nil {
		return Config{}, nil, err
	}

	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	overrides := map[string]string{}
	var warnings []string

	if set["password"] || set["pass"] {
		return Config{}, nil, errors.New("--password is no longer supported: webpty starts with the administrator password CHANGEME " +
			"and requires changing it at first sign-in")
	}
	if set["allowed-hosts"] || set["ah"] {
		warnings = append(warnings, "--allowed-hosts is deprecated and has no effect: webpty accepts only loopback hosts, "+
			"or the host of --public-origin when one is set")
	}
	if set["keepalive"] || set["k"] {
		warnings = append(warnings, "--keepalive is deprecated and has no effect: idle connections are probed every WEBPTY_WS_PING_INTERVAL")
	}

	if set["address"] {
		if set["port"] || set["p"] {
			return Config{}, nil, errors.New("use either --address or --port, not both")
		}
		if address == "" {
			return Config{}, nil, errors.New("--address must not be empty")
		}
		overrides["WEBPTY_ADDRESS"] = address
	}
	if set["port"] || set["p"] {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, nil, fmt.Errorf("--port %q must be a number from 1 to 65535", port)
		}
		current, _ := lookup("WEBPTY_ADDRESS")
		host, _, err := net.SplitHostPort(stringOr(current, "127.0.0.1:8000"))
		if err != nil {
			return Config{}, nil, fmt.Errorf("WEBPTY_ADDRESS: %w", err)
		}
		overrides["WEBPTY_ADDRESS"] = net.JoinHostPort(host, strconv.Itoa(n))
	}
	if set["database"] {
		if database == "" {
			return Config{}, nil, errors.New("--database must not be empty")
		}
		overrides["WEBPTY_DATABASE_PATH"] = database
	}
	if set["public-origin"] {
		overrides["WEBPTY_PUBLIC_ORIGIN"] = origin
	}
	if set["secure-cookies"] {
		overrides["WEBPTY_SECURE_COOKIES"] = strconv.FormatBool(secure)
	}

	commandSet := set["cmd"] || set["c"]
	if commandSet {
		if command == "" || strings.IndexByte(command, 0) >= 0 {
			return Config{}, nil, errors.New("--cmd must name an executable")
		}
		overrides["WEBPTY_COMMAND"] = command
	} else if flags.NArg() > 0 {
		return Config{}, nil, errors.New("command arguments require --cmd")
	}

	cfg, err := LoadFrom(func(key string) (string, bool) {
		if value, ok := overrides[key]; ok {
			return value, true
		}
		return lookup(key)
	})
	if err != nil {
		return Config{}, nil, err
	}
	if commandSet && flags.NArg() > 0 {
		cfg.CommandArgs = append([]string(nil), flags.Args()...)
	}
	return cfg, warnings, nil
}
