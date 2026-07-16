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
			"Saving the server URL once beats exporting PADDOCK_SERVER in every shell\n" +
			"(the env var still wins when set).",
		Action: func(_ context.Context, _ *cli.Command) error { return showConfig() },
		Commands: []*cli.Command{
			{
				Name:      "set",
				Usage:     "save a setting",
				ArgsUsage: "server <url>",
				Action: func(_ context.Context, c *cli.Command) error {
					if c.Args().First() != "server" || c.Args().Get(1) == "" {
						return cli.Exit("usage: paddock config set server <url>", 2)
					}
					cfg := loadConfig()
					cfg.Server = c.Args().Get(1)
					if err := saveConfig(cfg); err != nil {
						return err
					}
					fmt.Println("server:", cfg.Server)
					return nil
				},
			},
			{
				Name:      "unset",
				Usage:     "clear a setting",
				ArgsUsage: "server",
				Action: func(_ context.Context, c *cli.Command) error {
					if c.Args().First() != "server" {
						return cli.Exit("usage: paddock config unset server", 2)
					}
					cfg := loadConfig()
					cfg.Server = ""
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
	if c.Server == "" {
		fmt.Println("server: (unset)")
		return nil
	}
	fmt.Println("server:", c.Server)
	return nil
}
