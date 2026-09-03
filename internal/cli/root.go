// Package cli defines the Swarmfolio command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/liblaf/swarmfolio/assets"
	"github.com/liblaf/swarmfolio/internal/app"
	"github.com/liblaf/swarmfolio/internal/buildinfo"
	"github.com/liblaf/swarmfolio/internal/config"
	"github.com/liblaf/swarmfolio/internal/lock"
	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

type options struct {
	configPath string
	stdout     io.Writer
	stderr     io.Writer
}

func New(stdout, stderr io.Writer) *cobra.Command {
	options := &options{stdout: stdout, stderr: stderr}
	command := &cobra.Command{
		Use:           "swarmfolio",
		Short:         "Optimize an M-Team freeleech portfolio in qBittorrent",
		Version:       buildinfo.Current(),
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.PersistentFlags().StringVar(&options.configPath, "config", "", "configuration file (default: $XDG_CONFIG_HOME/swarmfolio/config.toml)")
	command.AddCommand(
		options.runCommand(),
		options.planCommand(),
		options.configCommand(),
		options.completionCommand(command),
		options.systemdCommand(),
	)
	return command
}

func (o *options) runCommand() *cobra.Command {
	var apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "run",
		Short: "Plan one optimization pass, and optionally apply it",
		RunE: func(command *cobra.Command, _ []string) error {
			return o.execute(command.Context(), apply, jsonOutput)
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "apply the plan to qBittorrent")
	command.Flags().BoolVar(&jsonOutput, "json", false, "write the report as JSON")
	return command
}

func (o *options) planCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "plan",
		Short: "Print one read-only optimization plan",
		RunE: func(command *cobra.Command, _ []string) error {
			return o.execute(command.Context(), false, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "write the report as JSON")
	return command
}

func (o *options) execute(ctx context.Context, apply, jsonOutput bool) error {
	if apply {
		runLock, err := lock.Acquire()
		if err != nil {
			return err
		}
		defer func() { _ = runLock.Close() }()
	}
	settings, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: settings.HTTPTimeout}
	qbt, err := qbittorrent.New(qbittorrent.Config{
		BaseURL: settings.QBittorrent.BaseURL, Username: settings.QBittorrent.Username,
		Password: settings.QBittorrent.Password, APIKey: settings.QBittorrent.APIKey,
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}
	mt, err := mteam.NewClient(mteam.Config{
		BaseURL: settings.MTeam.BaseURL, APIKey: settings.MTeam.APIKey,
		Mode: settings.MTeam.Mode, PageSize: settings.MTeam.PageSize, Pages: settings.MTeam.Pages,
		Timezone: settings.MTeam.Location.String(), HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}
	report, err := (app.Runner{Config: settings, QBittorrent: qbt, MTeam: mt}).Execute(ctx, apply)
	if err != nil {
		return err
	}
	if jsonOutput {
		encoder := json.NewEncoder(o.stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	writeReport(o.stdout, report)
	return nil
}

func (o *options) configCommand() *cobra.Command {
	command := &cobra.Command{Use: "config", Short: "Inspect or initialize configuration"}
	command.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the configuration path",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := o.configPath
			if path == "" {
				var err error
				path, err = config.DefaultPath()
				if err != nil {
					return err
				}
			}
			_, err := fmt.Fprintln(o.stdout, path)
			return err
		},
	})
	var force bool
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "Write an example configuration",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := o.configPath
			if path == "" {
				var err error
				path, err = config.DefaultPath()
				if err != nil {
					return err
				}
			}
			if err := writeFile(path, []byte(config.Example), 0o600, force); err != nil {
				return err
			}
			_, err := fmt.Fprintf(o.stdout, "Wrote %s\n", path)
			return err
		},
	}
	initCommand.Flags().BoolVar(&force, "force", false, "replace an existing configuration")
	command.AddCommand(initCommand)
	return command
}

func (o *options) completionCommand(root *cobra.Command) *cobra.Command {
	command := &cobra.Command{Use: "completion", Short: "Generate shell completion"}
	command.AddCommand(&cobra.Command{
		Use:   "fish",
		Short: "Generate Fish shell completion",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return root.GenFishCompletion(o.stdout, true)
		},
	})
	return command
}

func (o *options) systemdCommand() *cobra.Command {
	command := &cobra.Command{Use: "systemd", Short: "Print or install the user systemd units"}
	command.AddCommand(&cobra.Command{
		Use:       "print (service|timer)",
		Short:     "Print an embedded user unit",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"service", "timer"},
		RunE: func(_ *cobra.Command, args []string) error {
			if args[0] != "service" && args[0] != "timer" {
				return fmt.Errorf("unit must be service or timer, got %q", args[0])
			}
			data, err := assets.Files.ReadFile("systemd/swarmfolio." + args[0])
			if err != nil {
				return err
			}
			_, err = o.stdout.Write(data)
			return err
		},
	})
	var force bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the embedded units under $XDG_CONFIG_HOME/systemd/user",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			dir, err := os.UserConfigDir()
			if err != nil {
				return fmt.Errorf("resolve XDG config directory: %w", err)
			}
			dir = filepath.Join(dir, "systemd", "user")
			for _, name := range []string{"swarmfolio.service", "swarmfolio.timer"} {
				data, err := assets.Files.ReadFile("systemd/" + name)
				if err != nil {
					return err
				}
				if err := writeFile(filepath.Join(dir, name), data, 0o644, force); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(o.stdout, "Installed user units in %s\nRun: systemctl --user daemon-reload && systemctl --user enable --now swarmfolio.timer\n", dir)
			return err
		},
	}
	install.Flags().BoolVar(&force, "force", false, "replace existing unit files")
	command.AddCommand(install)
	return command
}

func writeFile(path string, data []byte, mode os.FileMode, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create directory for %q: %w", path, err)
	}
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists (use --force to replace it)", path)
		}
		return fmt.Errorf("create %q: %w", path, err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write %q: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %q: %w", path, closeErr)
	}
	return nil
}

func writeReport(writer io.Writer, report app.Report) {
	fmt.Fprintf(writer, "Mode: %s\n", report.Mode)
	fmt.Fprintf(writer, "Download disk: %s free of %s; reserve %s (%.1f%%)\n",
		formatBytes(report.Budget.FreeBytes), formatBytes(report.Budget.CapacityBytes),
		formatBytes(report.Budget.RequiredFreeBytes), report.Budget.MinimumFreePercent)
	fmt.Fprintf(writer, "Portfolio: %s now; %s limit; %s projected\n",
		formatBytes(report.Budget.UsedBytes), formatBytes(report.Budget.LimitBytes), formatBytes(report.ProjectedUsedBytes))
	for _, recovery := range report.Recoveries {
		fmt.Fprintf(writer, "Recovered: %s %s (%s)\n", recovery.Action, recovery.Name, shortHash(recovery.Hash))
	}
	for _, action := range report.Actions {
		verb := "Add"
		if action.Applied {
			verb = "Added"
		}
		fmt.Fprintf(writer, "%s: %s [M-Team %s, %s, %dL/%dS, score %.3f]\n",
			verb, action.Name, action.CandidateID, formatBytes(action.SizeBytes), action.Leechers, action.Seeders, action.Opportunity)
		for _, removal := range action.Removals {
			fmt.Fprintf(writer, "  Replace: %s (%s, %s)\n", removal.Name, shortHash(removal.Hash), formatBytes(removal.SizeBytes))
		}
	}
	if len(report.Actions) == 0 && len(report.Recoveries) == 0 {
		fmt.Fprintln(writer, "No changes selected.")
	}
	if report.SkippedWithoutExpiry > 0 {
		fmt.Fprintf(writer, "Skipped %d freeleech candidates without a verifiable expiry.\n", report.SkippedWithoutExpiry)
	}
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	divisor, exponent := int64(unit), 0
	for value := bytes / unit; value >= unit && exponent < 5; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(divisor), "KMGTPE"[exponent])
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
