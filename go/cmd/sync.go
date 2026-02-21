package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Force full resync from CloudKit",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := syncEngine.Sync(true); err != nil {
			return err
		}
		fmt.Println("✅ Sync complete.")
		return nil
	},
}
