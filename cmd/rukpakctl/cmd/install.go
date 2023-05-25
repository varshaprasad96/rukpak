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
		Long: `The install command helps in installation of an operator bundle on your cluster.
Currently it only supports registry v1 bundles`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ins.Name = args[0]

			if err := ins.Validate(); err != nil {
				log.Fatalf("Failed to validate input arguments: %s", err)
			}

			if err := ins.Complete(); err != nil {
				log.Fatalf("Failed to complete install options: %s", err)
			}

			if err := ins.Run(); err != nil {
				log.Fatalf("Failed to install bundle on cluster: %s", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ins.ProvisionerClassName, "provisioner-class", "core-rukpak-io-plain", "the name of the provisioner to be used.`.")
	cmd.Flags().StringVar(&ins.BundleDeploymentProvisionerClassName, "bd-provisioner-class", "core-rukpak-io-registry", "the name of the provisioner to be used for bundle deployment.`.")
	cmd.Flags().StringVar(&ins.ImageRef, "image-ref", "", "operator bundle image")
	cmd.Flags().StringVar(&ins.UnpackImage, "unpack-image", "", "image used to unpack contents of the operator bundle")
	return cmd
}
