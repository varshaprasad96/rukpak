package cmd

import (
	"log"

	"github.com/operator-framework/rukpak/cmd/rukpakctl/cmd/install"
	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	ins := &install.InstallOptions{}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Operator Lifecycle Manager in your cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			ins.Name = "prometheus"
			ins.ProvisionerClassName = "core-rukpak-io-plain"
			ins.BundleProvisionerClassName = "core-rukpak-io-registry"
			ins.ImageRef = "quay.io/operatorhubio/prometheus:v0.47.0--20220325T220130"

			if err := ins.Validate(); err != nil {
				log.Fatalf("Failed to install bundle: %s", err)
			}

			if err := ins.Complete(); err != nil {
				log.Fatalf("Failed to complete install bundle: %s", err)
			}

			if err := ins.Run(); err != nil {
				log.Fatalf("Failed to run install bundle: %s", err)
			}
			return nil
		},
	}

	return cmd
}
