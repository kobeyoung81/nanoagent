package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/agent"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"github.com/spf13/cobra"
)

func newHomeCmd() *cobra.Command {
	homeCmd := &cobra.Command{
		Use:   "home",
		Short: "Manage per-agent home directories",
	}
	homeCmd.AddCommand(newHomeReconcileCmd())
	return homeCmd
}

// newHomeReconcileCmd applies the image template to existing agent homes.
//
// A template-owned file is refreshed only while its content still matches what
// the platform wrote there (the current template, or an older version recorded
// in the manifest); a file the user edited is left alone and reported, so a
// template update can never clobber someone's AGENT.md. Runtime data
// (sessions/, media/, memory/) is not part of the manifest and is never touched.
func newHomeReconcileCmd() *cobra.Command {
	var dataRoot, onlyAgent, templateDir string
	var force, dryRun bool

	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Apply the image template to existing agent homes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if templateDir == "" {
				templateDir = config.DefaultTemplateDir
			}
			root := dataRoot
			if root == "" {
				root = config.DefaultDataRoot
			}
			if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(root, "~") {
				root = filepath.Join(home, strings.TrimPrefix(root, "~"))
			}

			manifest, err := agent.LoadTemplateManifest(templateDir)
			if err != nil {
				return err
			}
			if manifest == nil {
				return fmt.Errorf("no template manifest in %s", templateDir)
			}

			agentsDir := filepath.Join(root, "agents")
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				return fmt.Errorf("read agents dir %s: %w", agentsDir, err)
			}

			fmt.Printf("template version %d (%s)\n", manifest.Version, templateDir)
			visited := 0
			for _, e := range entries {
				if !e.IsDir() || (onlyAgent != "" && e.Name() != onlyAgent) {
					continue
				}
				agentDir := filepath.Join(agentsDir, e.Name(), "agent")
				if _, err := os.Stat(agentDir); err != nil {
					continue
				}
				visited++
				results, err := agent.ReconcileTemplate(manifest, templateDir, agentDir, force, dryRun)
				if err != nil {
					return fmt.Errorf("reconcile %s: %w", e.Name(), err)
				}
				for _, r := range results {
					fmt.Printf("  %-18s %-12s %s\n", e.Name(), r.Action, r.Path)
				}
			}
			if visited == 0 {
				fmt.Println("  (no agent homes found)")
			}
			if dryRun {
				fmt.Println("(dry run: nothing was written)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataRoot, "data-root", "", "agent data root (default: the derived data root)")
	cmd.Flags().StringVar(&onlyAgent, "agent", "", "only reconcile this home folder")
	cmd.Flags().StringVar(&templateDir, "template-dir", "", "image template directory (default "+config.DefaultTemplateDir+")")
	cmd.Flags().BoolVar(&force, "force", false, "also overwrite files the user edited")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	return cmd
}
