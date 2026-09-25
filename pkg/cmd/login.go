package cmd

import (
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/version"
)

// NewLoginCommand obtains a revocable, scoped CLI token (contract v1.12.0,
// kn-odqp) and stores it in ~/.kubenest/credentials.json (0600), keyed by
// control-plane URL.
//
// Default is the device flow: the CLI prints a short code, the human approves
// it in the console from any browser — which works from headless bastions and
// composes with enterprise IdPs — and the CLI polls until the token arrives.
// The CLI never sees the user's password, and the token is scoped to what the
// installer needs (clusters:read, clusters:register, bundles:read,
// install:report), nothing else.
//
// --token-stdin is the manual fallback for a token created in the console
// under CLI tokens.
//
// --ca-file exists because a self-hosted control plane — the one
// `kubenest platform install --control-plane` installs — issues its own CA,
// which no system trust store has ever seen. The PEM is trusted for this
// login and stored for every later command.
func NewLoginCommand() *cobra.Command {
	var (
		controlPlane string
		tokenStdin   bool
		caFile       string
	)

	cmd := &cobra.Command{
		Use:   "login --control-plane https://api.your-domain.com",
		Short: "Authenticate to your KubeNest control plane",
		Long: `Obtain a revocable CLI token from your control plane and store it in
~/.kubenest/credentials.json, readable only by you.

By default this starts a device authorization: approve the printed code in
your console from any browser, and the CLI receives a token scoped to what
the installer needs. Your password never passes through the CLI.

For automation, create a token in the console and pipe it to --token-stdin.

Pass --ca-file when the control plane serves a certificate no public root
signed: the PEM file is trusted for this login and remembered, so later
commands (adding a cluster to this control plane among them) verify it too.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			if controlPlane == "" {
				controlPlane = cfg.ControlPlaneURL
			}
			if controlPlane == "" {
				return fmt.Errorf("no control plane given: kubenest login --control-plane https://api.your-domain.com")
			}

			// Read and check the CA before anything is sent anywhere: a
			// mistyped path or a DER file has to fail as that, not as a TLS
			// failure halfway through a login.
			opts := []api.Option{}
			if caFile != "" {
				caPEM, err := readCACertFile(caFile)
				if err != nil {
					return err
				}
				cfg.ControlPlaneCA = string(caPEM)
				opts = append(opts, api.WithCACert(caPEM))
			}

			client, err := api.New(controlPlane, opts...)
			if err != nil {
				return err
			}

			var token string
			if tokenStdin {
				raw, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				token = strings.TrimSpace(string(raw))
				if token == "" {
					return fmt.Errorf("--token-stdin: no token on stdin (create one in the console under CLI tokens)")
				}
			} else {
				auth, err := client.StartDeviceAuth(cmd.Context(), "kubenest-cli "+version.Version)
				if err != nil {
					return err
				}

				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Confirm this device in your console:\n\n")
				if auth.VerificationURIComplete != "" {
					fmt.Fprintf(out, "    %s\n\n", auth.VerificationURIComplete)
					fmt.Fprintf(out, "or open %s and enter the code %s\n\n", auth.VerificationURI, auth.UserCode)
				} else {
					fmt.Fprintf(out, "    %s\n    Code: %s\n\n", auth.VerificationURI, auth.UserCode)
				}
				fmt.Fprintf(out, "Waiting for approval...\n")

				token, err = client.WaitForDeviceToken(cmd.Context(), auth)
				if err != nil {
					return err
				}
			}

			creds, err := config.LoadCredentials()
			if err != nil {
				return err
			}
			creds.Set(client.BaseURL(), token)
			if err := config.SaveCredentials(creds); err != nil {
				return fmt.Errorf("token obtained but could not be stored: %w", err)
			}

			// The control-plane URL is remembered so later commands and a
			// bare re-login need no flag. Any credential the old
			// password-JWT flow left in config.json is cleared.
			cfg.ControlPlaneURL = client.BaseURL()
			cfg.Token = ""
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s\n", client.BaseURL())

			// THE DEVICE FLOW'S OWN EXCHANGE IS THE PLACE THE FLOOR IS ASKED.
			// The CLI has just talked to this control plane and holds a fresh
			// token, so a refusal here costs the operator nothing and names the
			// only fix — upgrade the control plane — rather than leaving it to
			// surface later on a route the older control plane does not serve.
			//
			// --token-stdin IS DELIBERATELY NOT CHECKED. That path exists for a
			// console-created token and must reach NOTHING: an operator whose
			// control plane is unreachable, fenced or being restored still has
			// to be able to store the credential they hold. The check runs on
			// the next command that needs the control plane.
			if tokenStdin {
				return nil
			}
			client.SetToken(token)
			return checkControlPlaneForCommand(cmd.Context(), "login", client)
		},
	}

	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "control plane URL, e.g. https://api.your-domain.com")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read a console-created CLI token from stdin instead of the device flow")
	cmd.Flags().StringVar(&caFile, "ca-file", "", "PEM file of the control plane's own CA, when its certificate is not signed by a public root")
	return cmd
}

// readCACertFile reads a PEM file and refuses anything that is not at least
// one certificate, so a mistyped path or a DER file is reported as that
// rather than surfacing as a TLS failure later.
func readCACertFile(path string) ([]byte, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--ca-file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("--ca-file %s: no certificate found in it (expected PEM: -----BEGIN CERTIFICATE-----)", path)
	}
	return pemBytes, nil
}

// NewLogoutCommand discards the stored credential for the configured control
// plane. The token remains valid server-side until revoked in the console —
// logout says so rather than implying otherwise.
func NewLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Discard the stored control-plane credential",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			creds, err := config.LoadCredentials()
			if err != nil {
				return err
			}

			had := false
			if cfg.ControlPlaneURL != "" && creds.TokenFor(cfg.ControlPlaneURL) != "" {
				creds.Delete(cfg.ControlPlaneURL)
				had = true
			}
			if cfg.Token != "" { // legacy password-JWT storage
				cfg.Token = ""
				had = true
			}
			if err := config.SaveCredentials(creds); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if !had {
				fmt.Fprintln(out, "No stored credential.")
				return nil
			}
			fmt.Fprintln(out, "Logged out: the local credential is deleted.")
			fmt.Fprintln(out, "The token itself stays valid until you revoke it in the console under CLI tokens.")
			return nil
		},
	}
}
