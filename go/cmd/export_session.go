package cmd

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"icloud-reminders/cache"
)

var exportSessionCmd = &cobra.Command{
	Use:   "export-session [output_file]",
	Short: "Export session cookies to a tar.gz file",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		outputFile := "icloud-session.tar.gz"
		if len(args) > 0 {
			outputFile = args[0]
		}

		// Find session files in config dir
		entries, err := os.ReadDir(cache.ConfigDir)
		if err != nil {
			return fmt.Errorf("read config dir: %w", err)
		}

		var sessionFiles []string
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".session") || strings.HasSuffix(name, ".token") {
				sessionFiles = append(sessionFiles, filepath.Join(cache.ConfigDir, name))
			}
		}

		if len(sessionFiles) == 0 {
			fmt.Fprintln(os.Stderr, "❌ No session files found. Please login first.")
			os.Exit(1)
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
				Name:     filepath.Base(path),
				Size:     info.Size(),
				Mode:     0600,
				ModTime:  info.ModTime(),
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

		fmt.Printf("✅ Exported %d session file(s) to: %s\n", len(sessionFiles), outputFile)
		fmt.Println()
		fmt.Println("⚠️  WARNING: These cookies grant full iCloud access!")
		fmt.Println("   Only share with trusted parties.")
		fmt.Println("   Sessions may expire and require re-authentication.")
		return nil
	},
}
