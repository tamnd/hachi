// Command hachi is the hive: a conversation-first orchestrator for AI
// coding agents. Run it in a repo and start talking.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"

	"github.com/tamnd/hachi/adapter"
	_ "github.com/tamnd/hachi/adapter/codex"
	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/tui"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "hachi",
		Short: "a hive for AI coding agents",
		Long:  "hachi (蜂) wraps any AI coding agent in a glass hive:\none conversation in front, observable bees behind it.",
		RunE:  run,
	}
	root.Flags().StringP("dir", "d", "", "working directory for the session (default: cwd)")
	root.Flags().StringP("brain", "b", "", "agent to drive the session (default: first detected)")

	brains := &cobra.Command{
		Use:   "brains",
		Short: "list known agents and whether they are installed",
		RunE: func(cmd *cobra.Command, args []string) error {
			usable := map[string]bool{}
			for _, info := range adapter.Detect() {
				usable[info.Name] = true
			}
			for _, name := range adapter.Names() {
				mark := "not found"
				if usable[name] {
					mark = "ok"
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-10s %s\n", name, mark)
			}
			return nil
		},
	}
	root.AddCommand(brains)
	root.AddCommand(doctorCmd())

	if err := fang.Execute(context.Background(), root, fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	dir, _ := cmd.Flags().GetString("dir")
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	brain, _ := cmd.Flags().GetString("brain")
	if brain == "" {
		if usable := adapter.Detect(); len(usable) > 0 {
			brain = usable[0].Name
		}
	}
	if brain == "" {
		return fmt.Errorf("no agent found; install codex (https://github.com/openai/codex) or pass --brain")
	}

	hive, err := hiveHome()
	if err != nil {
		return err
	}
	j, err := journal.NewFiles(hive)
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()

	return tui.Run(engine.New(j), tui.Options{Dir: dir, Brain: brain, Brains: adapter.Names()})
}

// hiveHome resolves where the journal lives. HACHI_HOME points the hive
// somewhere else; demos and gates run on a throwaway one so the real
// journal never sees test sessions.
func hiveHome() (string, error) {
	if h := os.Getenv("HACHI_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hachi"), nil
}
