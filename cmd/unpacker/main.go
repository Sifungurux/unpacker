package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Sifungurux/unpacker/internal/unpacker"
)

// version is set via -ldflags "-X main.version=..." by goreleaser at release
// build time; it stays "dev" for local `go build`.
var version = "dev"

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
	var allowInsecureCredentials bool
	var withReferrers bool
	var maxTotalBytes int64
	var maxFileBytes int64
	var maxEntries int

	cmd := &cobra.Command{
		Use:     "unpacker IMAGE",
		Short:   "Pull and unpack OCI and Docker artifacts from a registry",
		Version: version,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			image := args[0]

			creds, err := unpacker.Resolve(configPath, public)
			if err != nil {
				return fmt.Errorf("credentials: %w", err)
			}

			cfg := &unpacker.Config{
				Image:                    image,
				OutputDir:                outputDir,
				AllowedTypes:             mediatypes,
				Insecure:                 insecure,
				AllowInsecureCredentials: allowInsecureCredentials,
				WithReferrers:            withReferrers,
				Creds:                    creds,
				Limits: unpacker.Limits{
					MaxTotalBytes: maxTotalBytes,
					MaxFileBytes:  maxFileBytes,
					MaxEntries:    maxEntries,
				},
			}

			ctx := context.Background()

			resolved, err := unpacker.Pull(ctx, cfg)
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}

			if err := unpacker.Unpack(cfg); err != nil {
				return fmt.Errorf("unpack: %w", err)
			}

			// result.json records what was resolved either way; FetchReferrers
			// writes it itself once it knows what it downloaded.
			if withReferrers {
				if _, err := unpacker.FetchReferrers(ctx, cfg, resolved); err != nil {
					return fmt.Errorf("referrers: %w", err)
				}
				return nil
			}

			return unpacker.WriteResult(cfg, &unpacker.Result{
				Image:     image,
				Digest:    resolved,
				Referrers: []unpacker.Referrer{},
			})
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output-dir", "o", ".", "Output directory")
	cmd.Flags().StringArrayVarP(&mediatypes, "mediatype", "m", []string{"flux", "helm"}, "Allowed mediatype (repeatable)")
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to dockerconfig.json for auth")
	cmd.Flags().BoolVarP(&public, "public", "p", false, "Pull from a public registry (no auth required)")
	cmd.Flags().BoolVarP(&insecure, "insecure", "k", false, "Skip TLS verification (self-signed certs)")
	cmd.Flags().BoolVar(&allowInsecureCredentials, "insecure-allow-credentials", false,
		"Permit sending credentials over plain HTTP (they travel unencrypted)")
	cmd.Flags().BoolVar(&withReferrers, "with-referrers", false,
		"Download artifacts attached to the image (SBOMs, attestations, signatures) and write result.json")
	cmd.Flags().Int64Var(&maxTotalBytes, "max-total-bytes", unpacker.DefaultMaxTotalBytes,
		"Maximum total bytes written when extracting an archive")
	cmd.Flags().Int64Var(&maxFileBytes, "max-file-bytes", unpacker.DefaultMaxFileBytes,
		"Maximum bytes written for a single file in an archive")
	cmd.Flags().IntVar(&maxEntries, "max-entries", unpacker.DefaultMaxEntries,
		"Maximum number of entries in an archive")

	return cmd
}
