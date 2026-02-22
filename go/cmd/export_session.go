package cmd

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"icloud-reminders/cache"
)

var exportSessionCmd = &cobra.Command{
	Use:   "export-session [output_file]",
	Short: "Export session and cache to a tar.gz file",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		outputFile := "icloud-session.tar.gz"
		if len(args) > 0 {
			outputFile = args[0]
		}

		// Export the known session files by path, not by extension scan.
		candidates := []string{cache.SessionFile, cache.CacheFile}
		var sessionFiles []string
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				sessionFiles = append(sessionFiles, p)
			}
		}

		if len(sessionFiles) == 0 {
			return fmt.Errorf("no session files found in %s — please run 'reminders auth' first", cache.ConfigDir)
		}

		out, err := os.Create(outputFile)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer out.Close()

		gz := gzip.NewWriter(out)
		defer gz.Close()
		tw := tar.NewWriter(gz)
		defer tw.Close()

		for _, path := range sessionFiles {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			hdr := &tar.Header{
				Name:    filepath.Base(path),
				Size:    info.Size(),
				Mode:    0600,
				ModTime: info.ModTime(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				f.Close()
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}

		fmt.Printf("✅ Exported %d file(s) to: %s\n", len(sessionFiles), outputFile)
		fmt.Println()
		fmt.Println("⚠️  WARNING: This archive grants full iCloud access!")
		fmt.Println("   Only share with trusted parties.")
		fmt.Println("   Sessions may expire and require re-authentication.")
		return nil
	},
}
