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
	// Distinct codes let a pipeline branch on why a run failed without
	// parsing stderr, which breaks the first time a message is reworded.
	// Anything unclassified is still 1.
	if err := rootCmd().Execute(); err != nil {
		os.Exit(unpacker.ExitCode(err))
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
	var platform string
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
				Platform:                 platform,
				Creds:                    creds,
				Verify:                   verifyCfg,
				Limits: unpacker.Limits{
					MaxTotalBytes: maxTotalBytes,
					MaxFileBytes:  maxFileBytes,
					MaxEntries:    maxEntries,
				},
			}

			ctx := context.Background()

			result := &unpacker.Result{Image: image, Referrers: []unpacker.Referrer{}}

			// result.json is written on every terminating path, failures
			// included. A run that dies without writing it leaves the
			// *previous* run's file in place, and a consumer reading the file
			// rather than the exit code reads a failure as the last success.
			fail := func(stage string, cause error) error {
				result.Error = &unpacker.ResultError{Stage: stage, Message: cause.Error()}
				if writeErr := unpacker.WriteResult(cfg, result); writeErr != nil {
					// Report the original failure; mention that the record of
					// it could not be written, because that is the thing a
					// consumer will now be missing.
					return fmt.Errorf("%s: %w (and writing result.json failed: %v)", stage, cause, writeErr)
				}
				return fmt.Errorf("%s: %w", stage, cause)
			}

			resolved, err := unpacker.Pull(ctx, cfg)
			if err != nil {
				return fail("pull", err)
			}
			result.Digest = resolved

			// Signatures are attached as referrers, so a verify run has to
			// fetch them whether or not --with-referrers was asked for. That
			// makes verification imply it: the bundle a run was accepted on is
			// worth keeping next to the artifact it vouches for.
			if withReferrers || verifyCfg.Requested() {
				fetched, refErr := unpacker.FetchReferrers(ctx, cfg, resolved)
				if refErr != nil {
					return fail("referrers", refErr)
				}
				result = fetched
			} else if err := unpacker.WriteResult(cfg, result); err != nil {
				return err
			}

			// Verification gates the unpack rather than following it: Unpack
			// publishes image/ by renaming a staging directory, so a refused
			// signature must be refused before that happens.
			if verifyCfg.Requested() {
				record, verifyErr := unpacker.Verify(cfg, resolved, result)
				result.Verification = record
				if verifyErr != nil {
					return fail("verify", verifyErr)
				}
				if writeErr := unpacker.WriteResult(cfg, result); writeErr != nil {
					return writeErr
				}
			}

			if err := unpacker.Unpack(cfg); err != nil {
				return fail("unpack", err)
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
	cmd.Flags().StringVar(&platform, "platform", "",
		"Platform to select from an image index, e.g. linux/arm64 (default: crane's, linux/amd64)")
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
