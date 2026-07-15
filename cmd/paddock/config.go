package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// configCmd implements `paddock config [set server <url> | unset server]`.
func configCmd(args []string) error {
	if len(args) == 0 {
		path, _ := configPath()
		c := loadConfig()
		fmt.Printf("config file: %s\n", path)
		if c.Server == "" {
			fmt.Println("server: (unset)")
		} else {
			fmt.Println("server:", c.Server)
		}
		return nil
	}
	switch args[0] {
	case "set":
		if len(args) != 3 || args[1] != "server" {
			return errors.New("usage: paddock config set server <url>")
		}
		c := loadConfig()
		c.Server = args[2]
		if err := saveConfig(c); err != nil {
			return err
		}
		fmt.Println("server:", c.Server)
		return nil
	case "unset":
		if len(args) != 2 || args[1] != "server" {
			return errors.New("usage: paddock config unset server")
		}
		c := loadConfig()
		c.Server = ""
		return saveConfig(c)
	default:
		return fmt.Errorf("unknown config command %q (want set or unset)", args[0])
	}
}
