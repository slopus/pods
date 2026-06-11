// Command pods is the Happy Pods CLI: deploy a folder of static files to a
// podbay server, get a URL, and poke at the tiny SQLite document store.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/slopus/pods/internal/client"
)

// version is the CLI version, overridable at build time via
// -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `Happy Pods - deploy a folder, get a URL.

Usage:

  pods <command> [arguments]

Commands:

  login [--endpoint URL] [--token T]   sign in to podbay.dev with GitHub (or --endpoint for self-hosted; --token to save an API token)
  logout                               delete the saved config
  status                               show endpoint, health, sites, and endpoint collections
  init [dir]                           scaffold a starter site
  deploy [dir] [--name N]              package a directory as tar.gz and deploy it
  list                                 list deployed sites
  rm <site> [--yes]                    delete a site (asks for confirmation)
  open <site>                          print the site URL (and open it on macOS)
  db <coll> list [--where k=v]... [--sort f] [--limit n] [--offset n] [--json]
  db <coll> get <id>
  db <coll> create <json|->
  db <coll> set <id> <json|->
  db <coll> patch <id> <json|->
  db <coll> rm <id>
  db <coll> drop [--yes]
  version                              print the CLI version
  help                                 show this help

Configuration (highest wins): --endpoint/--token flags, then the
PODS_ENDPOINT/PODS_TOKEN/PODS_SECRET environment variables, then ~/.config/pods/config.json.
`

func main() {
	err := run(os.Args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintf(os.Stderr, "pods: %v\n", err)
	os.Exit(1)
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "login":
		return cmdLogin(rest)
	case "logout":
		return cmdLogout(rest)
	case "status":
		return cmdStatus(rest)
	case "init":
		return cmdInit(rest)
	case "deploy":
		return cmdDeploy(rest)
	case "list":
		return cmdList(rest)
	case "rm":
		return cmdRm(rest)
	case "open":
		return cmdOpen(rest)
	case "db":
		return cmdDB(rest)
	case "version":
		fmt.Println("pods " + version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (run \"pods help\")", cmd)
	}
}

// newFlagSet returns a FlagSet for a subcommand with the shared
// --endpoint/--secret configuration flags registered.
func newFlagSet(name string) (fs *flag.FlagSet, endpoint, secret *string) {
	fs = flag.NewFlagSet("pods "+name, flag.ContinueOnError)
	endpoint = fs.String("endpoint", "", "server endpoint URL (overrides PODS_ENDPOINT and the config file)")
	secret = fs.String("secret", "", "API token (deprecated alias for --token)")
	fs.StringVar(secret, "token", "", "API token (overrides PODS_TOKEN, PODS_SECRET, and the config file)")
	return fs, endpoint, secret
}

// parseFlags parses args with flag's own output silenced so that parse
// errors are reported exactly once (by main, with the "pods: " prefix).
// On -h/--help it prints the flag defaults and returns flag.ErrHelp,
// which main treats as success.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Printf("Usage of %s:\n", fs.Name())
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
		return flag.ErrHelp
	}
	return err
}

// apiClient resolves the effective configuration and returns a client,
// requiring at least an endpoint.
func apiClient(flagEndpoint, flagSecret string) (*client.Client, error) {
	cfg, err := effectiveConfig(flagEndpoint, flagSecret)
	if err != nil {
		return nil, err
	}
	if cfg.Endpoint == "" {
		return nil, errors.New(`no endpoint configured (run "pods login" or set PODS_ENDPOINT)`)
	}
	c := client.New(cfg.Endpoint, cfg.Secret)
	maybeRefreshToken(c, cfg)
	return c, nil
}

// effectiveConfig loads the config file (if any) and applies the
// flag > env > file precedence.
func effectiveConfig(flagEndpoint, flagSecret string) (config, error) {
	var file config
	if path, err := configPath(); err == nil {
		file, err = loadConfigFile(path)
		if err != nil {
			return config{}, err
		}
	}
	return resolveConfig(flagEndpoint, flagSecret, os.Getenv, file), nil
}

// humanBytes formats a byte count for humans, e.g. "34.5 KiB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
