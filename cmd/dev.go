package cmd

import (
	"github.com/ente-io/cli/internal"
	"github.com/ente-io/cli/pkg"
	"github.com/spf13/cobra"
	"log"
	"os"
)

// versionCmd represents the version command
var devCommand = &cobra.Command{
	Use:   "dev",
	Short: "reserved for general development testing, do not use for general purpose as it",
}

var devExportCmd = &cobra.Command{
	Use:   "export",
	Short: "reserved for general development testing, do not use for general purpose as it",
	Run: func(cmd *cobra.Command, args []string) {
		recoverWithLog()
		skipVideo, _ := cmd.Flags().GetBool("skip-video")
		decrypt, _ := cmd.Flags().GetBool("should-decrypt")
		maxSize, _ := cmd.Flags().GetInt64("max-size")
		parallel, _ := cmd.Flags().GetInt("parallel")
		dir := os.Getenv("ENTE_CACHE_DIR")
		if dir == "" {
			log.Fatalf("This API is not intended for general use, please use the other command instead")
			return
		} else {
			parseDir, err := internal.ResolvePath(dir)
			if err != nil {
				log.Printf("Error parsing dir: %v", err)
			}
			dir = parseDir
		}
		if parallel <= 0 {
			log.Printf("parallel param is too low, setting to 4")
			parallel = 4
		} else if parallel > 10 {
			log.Printf("parallel param is too high, setting to 10")
			parallel = 10
		}
		ctrl.Export(&pkg.ExportParams{DevExport: &pkg.DevExport{
			Dir:           dir,
			SkipVideo:     skipVideo,
			ShouldDecrypt: decrypt,
			MaxSizeInMB:   maxSize,
			Email:         cmd.Flag("email").Value.String(),
		}})
	},
}

func init() {
	// add int param
	devExportCmd.Flags().Int64P("max-size", "m", 0, "max size in MB, if 0 then no limit")
	devExportCmd.Flags().BoolP("skip-video", "s", false, "skip video, only download photos")
	devExportCmd.Flags().BoolP("should-decrypt", "d", false, "true if files should be decrypted and false otherwise")
	devExportCmd.Flags().IntP("parallel", "p", 0, "parallel download param")
	devExportCmd.Flags().StringP("email", "e", "", "mention the email of the account to export")
	devExportCmd.MarkFlagRequired("email")
	rootCmd.AddCommand(devCommand)
	devCommand.AddCommand(devExportCmd)
}
