package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// NewAlertsCommand groups the instance's alert routing: where alerts go, and
// the dead-man's-switch URL the control plane beats to.
//
// WHY THESE ARE INSTANCE-SCOPED. Alerts are raised for every organisation on the
// control plane, and one of the destinations may be a channel the customer
// shares with KubeNest support — the route to us that Crest controls. There is
// deliberately no per-organisation destination here: that is a 1.3 question.
//
// The URL is a credential twice over (a Slack path sends to the channel, and the
// path alone is often the whole authorization), so the control plane stores it
// encrypted and never returns it, and nothing here prints a URL back.
func NewAlertsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "Where this instance's alerts are delivered",
		Long: `Manage the destinations every organisation's alerts are delivered to, and the
heartbeat URL the control plane beats when it and its delivery worker are
healthy.

Destinations are managed by the instance administrator, stored encrypted, and
never printed back: the control plane answers with their host, because a webhook
path is usually the credential itself.`,
	}
	cmd.AddCommand(newAddDestinationCommand())
	cmd.AddCommand(newListDestinationsCommand())
	cmd.AddCommand(newRemoveDestinationCommand())
	cmd.AddCommand(newTestAlertsCommand())
	cmd.AddCommand(newSetHeartbeatCommand())
	return cmd
}

func newAddDestinationCommand() *cobra.Command {
	var (
		destinationURL string
		format         string
	)
	cmd := &cobra.Command{
		Use:   "add-destination --url URL",
		Short: "Add a destination that receives every organisation's alerts",
		Long: `Add an instance alert destination.

The URL must be https: an alert payload discloses the fleet's problems and a
webhook path is a credential, so plaintext delivery is refused unless the host is
on the control plane's exact internal allow-list (for an intranet receiver).

An address the control plane must not connect to — loopback, link-local, a cloud
metadata address, or a private range — is refused here, when you add it, rather
than silently failing to deliver later.`,
		Example: `  kubenest alerts add-destination \
    --url https://hooks.slack.com/services/T000/B000/XXXXXXXX

  kubenest alerts add-destination --url https://alerts.example.com/kubenest --format generic`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if destinationURL == "" {
				return fmt.Errorf("--url is required")
			}
			return runAddDestination(cmd.Context(), cmd.OutOrStdout(), destinationURL, format)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&destinationURL, "url", "", "destination URL; https (required)")
	fs.StringVar(&format, "format", "generic", "wire format: generic or slack")
	return cmd
}

func newListDestinationsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-destinations",
		Short: "List the instance's alert destinations",
		Long: `List every alert destination.

Hosts only. A destination's URL is not shown, here or anywhere: the control
plane never returns it, because a webhook path routinely IS the credential.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runListDestinations(cmd.Context(), cmd.OutOrStdout())
		},
	}
	return cmd
}

func newRemoveDestinationCommand() *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "remove-destination --id ID",
		Short: "Remove an alert destination",
		Long: `Remove a destination and its undelivered alerts.

The id is the one ` + "`kubenest alerts list-destinations`" + ` prints. Removing a destination
discards the alerts still queued for it — they cannot be delivered anywhere else.`,
		Example: `  kubenest alerts remove-destination --id 0192f0c4-...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return fmt.Errorf("--id is required (see `kubenest alerts list-destinations`)")
			}
			return runRemoveDestination(cmd.Context(), cmd.OutOrStdout(), id)
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "destination id (required)")
	return cmd
}

func newTestAlertsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Send a test alert to every destination",
		Long: `Send a test alert to every enabled destination and report each answer.

This goes through the same path a real alert takes — the same address checks, the
same timeouts, the same refusal to follow a redirect — so a destination that
accepts the test will accept an alert.

Exits non-zero when a destination did not accept the test.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTestAlerts(cmd.Context(), cmd.OutOrStdout())
		},
	}
	return cmd
}

func newSetHeartbeatCommand() *cobra.Command {
	var heartbeatURL string
	cmd := &cobra.Command{
		Use:   "set-heartbeat --url URL",
		Short: "Set the dead-man's-switch URL the control plane beats",
		Long: `Store the heartbeat URL.

The control plane requests it after an evaluation sweep succeeds AND the alert
delivery worker has completed a bounded cycle inside its deadline — an empty
queue counts. A stopped worker or a backlog past its critical age stops the
beats even while the fleet is quiet, so any dead-man's-switch service you already
run will notice. Point it at one you control; KubeNest hosts nothing for it.`,
		Example: `  kubenest alerts set-heartbeat --url https://hc-ping.com/xxxxxxxx-xxxx-xxxx`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if heartbeatURL == "" {
				return fmt.Errorf("--url is required")
			}
			client, err := controlPlaneClient()
			if err != nil {
				return err
			}
			if err := client.SetHeartbeat(cmd.Context(), heartbeatURL); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Heartbeat URL stored. The control plane will beat it when it is healthy.")
			return nil
		},
	}
	cmd.Flags().StringVar(&heartbeatURL, "url", "", "heartbeat URL; https (required)")
	return cmd
}

func runAddDestination(ctx context.Context, out io.Writer, destinationURL, format string) error {
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}
	destination, err := client.AddAlertDestination(ctx, destinationURL, format)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Added alert destination %s (%s).\n", destination.URLHost, destination.Format)
	fmt.Fprintf(out, "id: %s\n", destination.ID)
	fmt.Fprintln(out, "Every organisation's alerts on this instance are delivered to it.")
	return nil
}

func runListDestinations(ctx context.Context, out io.Writer) error {
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}
	destinations, err := client.ListAlertDestinations(ctx)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		fmt.Fprintln(out, "No alert destinations are configured: alerts are being recorded and NOT delivered.")
		fmt.Fprintln(out, "Add one with `kubenest alerts add-destination --url https://...`.")
		return nil
	}
	for _, destination := range destinations {
		state := "enabled"
		if !destination.Enabled {
			state = "disabled"
		}
		fmt.Fprintf(out, "%s  %-8s %-8s added %s\n",
			destination.ID, destination.Format, state, destination.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(out, "    host: %s\n", destination.URLHost)
	}
	return nil
}

func runRemoveDestination(ctx context.Context, out io.Writer, id string) error {
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}
	if err := client.RemoveAlertDestination(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed alert destination %s. Alerts still queued for it were discarded.\n", id)
	return nil
}

func runTestAlerts(ctx context.Context, out io.Writer) error {
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}
	result, err := client.TestAlertDestinations(ctx)
	if err != nil {
		return err
	}
	if len(result.Destinations) == 0 {
		fmt.Fprintln(out, "No alert destinations are configured, so nothing was tested.")
		return nil
	}
	for _, destination := range result.Destinations {
		if destination.Delivered {
			fmt.Fprintf(out, "%s (%s): delivered\n", destination.URLHost, destination.Format)
			continue
		}
		fmt.Fprintf(out, "%s (%s): FAILED — %s\n", destination.URLHost, destination.Format, destination.Error)
	}
	var problems []string
	for _, destination := range result.Destinations {
		if !destination.Delivered {
			problems = append(problems, destination.URLHost)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d destination(s) did not accept the test alert: %s",
			len(problems), strings.Join(problems, ", "))
	}
	fmt.Fprintf(out, "%d destination(s) accepted the test alert.\n", result.Delivered)
	return nil
}
