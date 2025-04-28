package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/term"
)

func NewLoginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Login to Kubenest",
		RunE: func(cmd *cobra.Command, args []string) error {
			defaultAPIURL := "https://api.kubenest.io"
			fmt.Printf("Enter API URL [%s]: ", defaultAPIURL)
			apiURL, err := term.ReadLine()
			if err != nil {
				return err
			}
			if apiURL == "" {
				apiURL = defaultAPIURL
			}
			cfg, _ := config.LoadConfig()
			cfg.APIURL = apiURL
			config.SaveConfig(cfg)

			fmt.Print("Enter email: ")
			email, err := term.ReadLine()
			if err != nil {
				return err
			}

			fmt.Print("Enter password: ")
			password, err := term.ReadPassword()
			if err != nil {
				return err
			}

			client, _ := api.NewClient()
			loginResp, err := client.Login(email, password)
			if err != nil {
				return fmt.Errorf("login failed: %v", err)
			}
			cfg.Token = loginResp.Token
			config.SaveConfig(cfg)

			fmt.Println("Successfully logged in!")
			return nil
		},
	}
	return cmd
}
