package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/term"
)

// NewLoginCommand authenticates against the control plane and stores the
// credential in ~/.kubenest/config.json (0600).
//
// Today the credential is the control plane's bearer JWT obtained with email
// and password. When the control plane ships the revocable, scoped CLI token
// (kn-odqp), only the api.Client.Login exchange changes; storage and every
// caller stay as they are.
func NewLoginCommand() *cobra.Command {
	var (
		controlPlane  string
		email         string
		passwordStdin bool
	)

	cmd := &cobra.Command{
		Use:   "login --control-plane https://api.your-domain.com",
		Short: "Authenticate to your KubeNest control plane",
		Long: `Authenticate to your KubeNest control plane and store the credential in
~/.kubenest/config.json, readable only by you.

Non-interactive use: pass --email and pipe the password to --password-stdin.
The password itself is never accepted as a flag — flags leak into shell
history and process listings.`,
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

			client, err := api.New(controlPlane)
			if err != nil {
				return err
			}

			if email == "" {
				fmt.Fprint(cmd.OutOrStdout(), "Email: ")
				email, err = term.ReadLine()
				if err != nil {
					return err
				}
			}

			var password string
			switch {
			case passwordStdin:
				raw, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				password = strings.TrimRight(string(raw), "\r\n")
			default:
				fmt.Fprint(cmd.OutOrStdout(), "Password: ")
				password, err = term.ReadPassword()
				if err != nil {
					return err
				}
			}
			if email == "" || password == "" {
				return fmt.Errorf("email and password are required")
			}

			tok, err := client.Login(cmd.Context(), email, password)
			if err != nil {
				return err
			}
			client.SetToken(tok.AccessToken)

			cfg.ControlPlaneURL = client.BaseURL()
			cfg.Token = tok.AccessToken
			cfg.UserEmail = email
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("credential obtained but could not be stored: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s as %s\n", cfg.ControlPlaneURL, email)
			return nil
		},
	}

	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "control plane URL, e.g. https://api.your-domain.com")
	cmd.Flags().StringVar(&email, "email", "", "email (for non-interactive use)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin (for non-interactive use)")
	return cmd
}

// NewLogoutCommand discards the stored credential.
func NewLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Discard the stored control-plane credential",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfg.Token = ""
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
}
