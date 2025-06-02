package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/term"
)

func NewLoginCommand() *cobra.Command {
	var (
		email    string
		password string
		apiURL   string
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Login to Kubenest",
		RunE: func(cmd *cobra.Command, args []string) error {
			defaultAPIURL := "https://api.kubenest.io"

			// Handle API URL
			if apiURL == "" {
				fmt.Printf("Enter API URL [%s]: ", defaultAPIURL)
				inputURL, err := term.ReadLine()
				if err != nil {
					return err
				}
				if inputURL == "" {
					apiURL = defaultAPIURL
				} else {
					apiURL = inputURL
				}
			}

			cfg, _ := config.LoadConfig()
			cfg.APIURL = apiURL
			config.SaveConfig(cfg)

			// Handle email
			if email == "" {
				fmt.Print("Enter email: ")
				inputEmail, err := term.ReadLine()
				if err != nil {
					return err
				}
				email = inputEmail
			}

			// Handle password
			if password == "" {
				fmt.Print("Enter password: ")
				inputPassword, err := term.ReadPassword()
				if err != nil {
					return err
				}
				password = inputPassword
			}

			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			loginResp, err := client.Login(email, password)
			if err != nil {
				return fmt.Errorf("login failed: %v", err)
			}
			cfg.Token = loginResp.Token
			client.SetToken(cfg.Token)
			fmt.Printf("Token being used for GetUser: %q\n", cfg.Token)

			// Fetch user info and store in config
			userInfo, err := client.GetUser(cmd.Context())
			if err != nil {
				fmt.Printf("Failed to fetch user info: %v\n", err)
			} else {
				cfg.UserEmail = userInfo.Email
				cfg.UserFirstName = userInfo.FirstName
				cfg.UserLastName = userInfo.LastName
			}

			config.SaveConfig(cfg)

			fmt.Println("Successfully logged in!")
			return nil
		},
	}

	// Add flags for non-interactive usage
	cmd.Flags().StringVar(&email, "email", "", "Email for login (for non-interactive use)")
	cmd.Flags().StringVar(&password, "password", "", "Password for login (for non-interactive use)")
	cmd.Flags().StringVar(&apiURL, "api-url", "", "API URL (for non-interactive use)")

	return cmd
}
