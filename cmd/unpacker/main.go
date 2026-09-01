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
	var maxReferrers int
	var verifyIdentity, verifyIssuer, verifyKey string
	var verifyTrustedRoot, verifyTUFMirror, verifyTUFRoot string

	cmd := &cobra.Command{
		Use:     "unpacker IMAGE",
		Short:   "Pull and unpack OCI and Docker artifacts from a registry",
		Version: version,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			image := args[0]

			verifyCfg := unpacker.VerifyConfig{
				CosignIdentity:   verifyIdentity,
				CosignOIDCIssuer: verifyIssuer,
				CosignKeyPath:    verifyKey,
				TrustedRootPath:  verifyTrustedRoot,
				TUFMirror:        verifyTUFMirror,
				TUFRootPath:      verifyTUFRoot,
			}
			// Before anything that touches the network: a contradictory flag
			// set should say so, not fail later on credentials and leave the
			// real mistake unmentioned.
			if err := verifyCfg.Validate(); err != nil {
				return err
			}

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
				MaxReferrers:             maxReferrers,
				Creds:                    creds,
				Verify:                   verifyCfg,
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

			// Signatures are attached as referrers, so a verify run has to
			// fetch them whether or not --with-referrers was asked for. That
			// makes verification imply it: the bundle a run was accepted on is
			// worth keeping next to the artifact it vouches for.
			result := &unpacker.Result{Image: image, Digest: resolved, Referrers: []unpacker.Referrer{}}
			if withReferrers || verifyCfg.Requested() {
				result, err = unpacker.FetchReferrers(ctx, cfg, resolved)
				if err != nil {
					return fmt.Errorf("referrers: %w", err)
				}
			} else if err := unpacker.WriteResult(cfg, result); err != nil {
				return err
			}

			// Verification gates the unpack rather than following it: Unpack
			// publishes image/ by renaming a staging directory, so a refused
			// signature must be refused before that happens.
			if verifyCfg.Requested() {
				record, verifyErr := unpacker.Verify(cfg, resolved, result)
				result.Verification = record
				if writeErr := unpacker.WriteResult(cfg, result); writeErr != nil {
					return writeErr
				}
				if verifyErr != nil {
					return fmt.Errorf("verify: %w", verifyErr)
				}
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
	cmd.Flags().IntVar(&maxReferrers, "max-referrers", unpacker.DefaultMaxReferrers,
		"Maximum number of referrers to download for one image")
	cmd.Flags().StringVar(&verifyIdentity, "verify-cosign-identity", "",
		"Verify a cosign signature keylessly: regex the Fulcio certificate SAN must match")
	cmd.Flags().StringVar(&verifyIssuer, "verify-cosign-oidc-issuer", "",
		"OIDC issuer the signing certificate must come from (required with --verify-cosign-identity)")
	cmd.Flags().StringVar(&verifyKey, "verify-cosign-key", "",
		"Verify a cosign signature against this public key instead of a certificate identity")
	cmd.Flags().StringVar(&verifyTrustedRoot, "verify-trusted-root", "",
		"Path to a trusted_root.json for a private Sigstore cluster")
	cmd.Flags().StringVar(&verifyTUFMirror, "verify-tuf-mirror", "",
		"TUF repository URL for a private Sigstore cluster")
	cmd.Flags().StringVar(&verifyTUFRoot, "verify-tuf-root", "",
		"Path to the TUF bootstrap root.json (used with --verify-tuf-mirror)")

	return cmd
}
