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

var importSessionCmd = &cobra.Command{
	Use:   "import-session <input_file>",
	Short: "Import session cookies from a tar.gz file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		inputFile := args[0]

		if _, err := os.Stat(inputFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "❌ File not found: %s\n", inputFile)
			os.Exit(1)
		}

		if err := os.MkdirAll(cache.ConfigDir, 0700); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}

		f, err := os.Open(inputFile)
		if err != nil {
			return err
		}
		defer f.Close()

		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gzip read: %w", err)
		}
		defer gz.Close()

		tr := tar.NewReader(gz)
		var extracted []string

		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			// Security: only extract .session and .token files, no path traversal
			name := filepath.Base(hdr.Name)
			if !strings.HasSuffix(name, ".session") && !strings.HasSuffix(name, ".token") {
				continue
			}
			if strings.Contains(name, "/") || strings.Contains(name, "..") {
				continue
			}

			outPath := filepath.Join(cache.ConfigDir, name)
			out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
			extracted = append(extracted, name)
		}

		fmt.Printf("✅ Imported %d session file(s):\n", len(extracted))
		for _, name := range extracted {
			fmt.Printf("   - %s\n", name)
		}
		fmt.Println()
		fmt.Println("ℹ️  Session imported. Run 'reminders auth --force' if re-authentication is needed.")
		return nil
	},
}
