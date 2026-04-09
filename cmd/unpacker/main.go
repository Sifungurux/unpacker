package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/energinet/unpacker/internal/unpacker"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var outputDir string
	var mediatypes []string
	var configPath string
	var public bool
	var insecure bool

	cmd := &cobra.Command{
		Use:   "unpacker IMAGE",
		Short: "Pull and unpack OCI and Docker artifacts from a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			image := args[0]

			creds, err := unpacker.Resolve(configPath, public)
			if err != nil {
				return fmt.Errorf("credentials: %w", err)
			}

			cfg := &unpacker.Config{
				Image:        image,
				OutputDir:    outputDir,
				AllowedTypes: mediatypes,
				Insecure:     insecure,
				Creds:        creds,
			}

			if err := unpacker.Pull(context.Background(), cfg); err != nil {
				return fmt.Errorf("pull: %w", err)
			}

			if err := unpacker.Unpack(cfg); err != nil {
				return fmt.Errorf("unpack: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output-dir", "o", ".", "Output directory")
	cmd.Flags().StringArrayVarP(&mediatypes, "mediatype", "m", []string{"flux", "helm"}, "Allowed mediatype (repeatable)")
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to dockerconfig.json for auth")
	cmd.Flags().BoolVarP(&public, "public", "p", false, "Pull from a public registry (no auth required)")
	cmd.Flags().BoolVarP(&insecure, "insecure", "k", false, "Skip TLS verification (self-signed certs)")

	return cmd
}
