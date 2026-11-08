package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/vfs"
)

func newResolveCmd() *cobra.Command {
	var asJSON bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "resolve <path>",
		Short: "Print the uuid and CDN URL of a path in a mount",
		Long: "Tells what a path in a running mount stands for: the uuid and CDN URL of an\n" +
			"uploaded file or variant, or that a write is still pending. NFS on macOS has no\n" +
			"xattrs, so this is how a script learns the uuid a cp got. --wait blocks until a\n" +
			"pending upload finishes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sock, rel, err := mountOf(args[0])
			if err != nil {
				return err
			}
			deadline := time.Now().Add(wait)
			var res resolved
			for {
				res = resolved{}
				if err := ctl.Call(sock, http.MethodPost, "/resolve", map[string]string{"path": rel}, &res); err != nil {
					return fmt.Errorf("%s: %w", args[0], err)
				}
				if res.State != vfs.StatePending || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			return printResolved(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait this long for a pending upload")
	return cmd
}

func printResolved(w io.Writer, r resolved) error {
	rows := [][2]string{{"path", r.Path}, {"state", r.State}}
	if r.UUID != "" {
		rows = append(rows, [2]string{"uuid", r.UUID}, [2]string{"cdn_url", r.CDNURL})
	}
	if r.Spool != "" {
		rows = append(rows, [2]string{"spool", r.Spool})
	}
	if r.Error != "" {
		rows = append(rows, [2]string{"error", r.Error})
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%-8s %s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

// mountOf finds the running mount a path is in and the path relative to its
// root. The path need not exist yet; its directory must.
func mountOf(p string) (sock, rel string, err error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(dir, filepath.Base(abs))
	}
	reg, err := ctl.RegistryDir()
	if err != nil {
		return "", "", err
	}
	mounts, err := ctl.Mounts(reg)
	if err != nil {
		return "", "", err
	}
	best := -1
	for i, m := range mounts {
		dir := m.MountDir
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		if (abs == dir || strings.HasPrefix(abs, dir+string(os.PathSeparator))) &&
			(best < 0 || len(dir) > len(mounts[best].MountDir)) {
			best, rel = i, strings.TrimPrefix(strings.TrimPrefix(abs, dir), string(os.PathSeparator))
		}
	}
	if best < 0 {
		return "", "", errors.New(p + " is not in a running ucfs mount")
	}
	return mounts[best].Socket, filepath.ToSlash(rel), nil
}
