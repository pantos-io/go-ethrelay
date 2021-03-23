// This file contains logic executed if the command "verify" is typed in.
// Authors: Marten Sigwart, Philipp Frauenthaler

package cmd

import (
	"github.com/spf13/cobra"
)

var verifyFlagSrcChain uint8
var verifyFlagDestChain uint8

// verifyCmd represents the verify command
var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verifies a transaction or a block from the source chain on the verifying chain",
	Long:  `Verifies a transaction or a block from the source chain on the verifying chain`,
}

func init() {
	rootCmd.AddCommand(verifyCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// verifyCmd.PersistentFlags().String("foo", "", "A help for foo")

	verifyCmd.PersistentFlags().Uint8Var(&verifyFlagSrcChain, "target", 0, "target chain")
	verifyCmd.PersistentFlags().Uint8Var(&verifyFlagDestChain, "chain", 1, "verifying chain")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// verifyCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
