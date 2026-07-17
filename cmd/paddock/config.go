package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
)

// cliConfig is the developer's saved CLI configuration: set once with
// `paddock config set server <url>` instead of exporting an env var in
// every shell. Plain JSON in one file, so platform teams can also drop it
// in place via dotfiles/MDM.
type cliConfig struct {
	Server string `json:"server,omitempty"`
	// Token authenticates this developer to the control-plane API. The file
	// is written 0600 — it is a credential, and the developer's own.
	Token string `json:"token,omitempty"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "paddock", "config.json"), nil
}

func loadConfig() cliConfig {
	var c cliConfig
	path, err := configPath()
	if err != nil {
		return c
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	json.Unmarshal(raw, &c) // a broken file behaves like an empty one
	return c
}

func saveConfig(c cliConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// configCmd implements `paddock config [set|unset] server [<url>]`.
func configCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "show or save CLI settings",
		Description: "With no arguments, prints the current settings and where they live.\n" +
			"Saving the server URL and token once beats exporting PADDOCK_SERVER and\n" +
			"PADDOCK_TOKEN in every shell (the env vars still win when set).\n\n" +
			"Settings: server <url>, token <token>",
		Action: func(_ context.Context, _ *cli.Command) error { return showConfig() },
		Commands: []*cli.Command{
			{
				Name:      "set",
				Usage:     "save a setting",
				ArgsUsage: "server <url> | token <token>",
				Action: func(_ context.Context, c *cli.Command) error {
					key, value := c.Args().First(), c.Args().Get(1)
					if value == "" {
						return cli.Exit("usage: paddock config set (server <url>|token <token>)", 2)
					}
					cfg := loadConfig()
					switch key {
					case "server":
						cfg.Server = value
					case "token":
						cfg.Token = value
					default:
						return cli.Exit("unknown setting "+key+" (want: server, token)", 2)
					}
					if err := saveConfig(cfg); err != nil {
						return err
					}
					return showConfig()
				},
			},
			{
				Name:      "unset",
				Usage:     "clear a setting",
				ArgsUsage: "server | token",
				Action: func(_ context.Context, c *cli.Command) error {
					cfg := loadConfig()
					switch c.Args().First() {
					case "server":
						cfg.Server = ""
					case "token":
						cfg.Token = ""
					default:
						return cli.Exit("usage: paddock config unset (server|token)", 2)
					}
					return saveConfig(cfg)
				},
			},
		},
	}
}

func showConfig() error {
	path, _ := configPath()
	c := loadConfig()
	fmt.Printf("config file: %s\n", path)
	fmt.Println("server:", orUnset(c.Server))
	// Never the token itself: this prints in terminals, screen shares and
	// bug reports.
	fmt.Println("token:", orUnset(redact(c.Token)))
	return nil
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// redact keeps enough of a token to recognise which one is saved, and not
// enough to use it.
func redact(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "********"
	}
	return token[:4] + "…" + token[len(token)-4:]
}
