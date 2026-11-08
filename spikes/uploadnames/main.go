// Command uploadnames is Spike D in docs/spikes.md: it uploads files with
// awkward names to the uploadcare-go-test project, reports what the server
// kept, and deletes every upload. It is not part of ucfs.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/uploadcare/uploadcare-go/v2/file"
	"github.com/uploadcare/uploadcare-go/v2/project"
	"github.com/uploadcare/uploadcare-go/v2/ucare"
	"github.com/uploadcare/uploadcare-go/v2/upload"

	"github.com/alxgsv/ucfs/internal/config"
)

const testProject = "uploadcare-go-test"

type upCase struct {
	label string
	name  string
	data  []byte
	store string
	multi bool
}

func main() {
	ledger := flag.String("ledger", "", "file that records every uploaded uuid (required)")
	cleanupOnly := flag.Bool("cleanup-only", false, "delete the uuids in -ledger and exit")
	only := flag.String("only", "", "run only the case with this label")
	flag.Parse()
	if *ledger == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(context.Background(), *ledger, *cleanupOnly, *only); err != nil {
		fmt.Fprintln(os.Stderr, "uploadnames:", err)
		os.Exit(1)
	}
}

type clients struct {
	up    upload.Service
	files file.Service
	proj  project.Service
}

func run(ctx context.Context, ledger string, cleanupOnly bool, only string) error {
	cfg, err := config.Load(config.Options{Project: testProject})
	if err != nil {
		return err
	}
	if cfg.Project != testProject {
		return fmt.Errorf("refusing to run against project %q", cfg.Project)
	}
	creds := ucare.APICreds{PublicKey: cfg.PublicKey, SecretKey: cfg.SecretKey}
	ucfg, err := ucare.NewConfig(creds, ucare.WithRetry(&ucare.RetryConfig{MaxRetries: 5, MaxWaitSeconds: 60}))
	if err != nil {
		return err
	}
	uc, err := ucare.NewClient(creds, ucfg)
	if err != nil {
		return err
	}
	c := clients{up: upload.NewService(uc), files: file.NewService(uc), proj: project.NewService(uc)}

	if !cleanupOnly {
		if err := c.probe(ctx, ledger, only); err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
		}
	}
	return c.cleanup(ctx, ledger)
}

func (c clients) probe(ctx context.Context, ledger, only string) error {
	pinfo, err := c.proj.Info(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("project autostore_enabled=%v\n\n", pinfo.AutostoreEnabled)

	lf, err := os.OpenFile(ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()

	body := []byte("ucfs spike D\n")
	big := bytes.Repeat([]byte("0123456789abcdef"), (11<<20)/16)
	cases := []upCase{
		{label: "ascii", name: "plain.txt"},
		{label: "spaces", name: "with  two spaces .txt"},
		{label: "nfc", name: "caf\u00e9.txt"},
		{label: "nfd", name: "cafe\u0301.txt"},
		{label: "cyrillic", name: "привет мир.txt"},
		{label: "cjk", name: "日本語.txt"},
		{label: "emoji", name: "emoji 🙂.txt"},
		{label: "quote", name: `say "hi".txt`},
		{label: "backslash", name: `back\slash.txt`},
		{label: "semicolon", name: "a;b=c.txt"},
		{label: "slash", name: "dir/sub.txt"},
		{label: "dotfile", name: ".hidden"},
		{label: "long-230B", name: strings.Repeat("x", 226) + ".txt"},
		{label: "empty", name: "empty.txt", data: []byte{}},
		{label: "store=0", name: "store0.txt", store: upload.ToStoreFalse},
		{label: "store=auto", name: "storeauto.txt", store: upload.ToStoreAuto},
		{label: "multipart-nfd", name: "multi cafe\u0301 🙂 \"q\".bin", data: big, multi: true},
		{label: "multipart-bs", name: `multi back\slash.bin`, data: big, multi: true},
		{label: "multipart-slash", name: "multi dir/sub.bin", data: big, multi: true},
	}
	fmt.Printf("%-14s %-6s %-9s %-7s %-9s %s\n", "case", "kept", "is_ready", "stored", "size", "server original_filename (upload API filename)")
	for _, tc := range cases {
		if only != "" && tc.label != only {
			continue
		}
		if tc.data == nil {
			tc.data = body
		}
		if tc.store == "" {
			tc.store = upload.ToStoreTrue
		}
		id, err := c.upload(ctx, tc)
		if err != nil {
			fmt.Printf("%-14s upload error: %v\n", tc.label, err)
			continue
		}
		if _, err := fmt.Fprintln(lf, id); err != nil {
			return err
		}
		info, err := c.waitReady(ctx, id)
		if err != nil {
			fmt.Printf("%-14s %s info error: %v\n", tc.label, id, err)
			continue
		}
		upInfo, err := c.up.FileInfo(ctx, id)
		if err != nil {
			return err
		}
		kept := "yes"
		if info.OriginalFileName != tc.name {
			kept = "NO"
		}
		fmt.Printf("%-14s %-6s %-9v %-7v %-9d %q (%q)\n", tc.label, kept, info.IsReady, info.StoredAt != nil,
			info.Size, info.OriginalFileName, upInfo.FileName)
		if kept == "NO" {
			fmt.Printf("%14s sent % x\n%14s got  % x\n", "", []byte(tc.name), "", []byte(info.OriginalFileName))
		}
		if info.Metadata["ucfs:test"] != "1" {
			fmt.Printf("%14s metadata ucfs:test missing: %v\n", "", info.Metadata)
		}
	}
	return nil
}

func (c clients) upload(ctx context.Context, tc upCase) (string, error) {
	meta := map[string]string{"ucfs:test": "1"}
	if !tc.multi {
		return c.up.File(ctx, upload.FileParams{
			Data: bytes.NewReader(tc.data), Name: tc.name, ContentType: "text/plain",
			ToStore: ucare.String(tc.store), Metadata: meta,
		})
	}
	res, err := c.up.Multipart(ctx, upload.MultipartParams{
		FileName: tc.name, Size: int64(len(tc.data)), ContentType: "application/octet-stream",
		Data: bytes.NewReader(tc.data), ToStore: ucare.String(tc.store), Metadata: meta,
	})
	if err != nil {
		return "", err
	}
	select {
	case fi := <-res.Done():
		return fi.ID, nil
	case err := <-res.Error():
		return "", err
	}
}

// waitReady polls REST file info because storing and readiness are asynchronous.
func (c clients) waitReady(ctx context.Context, id string) (file.Info, error) {
	var info file.Info
	var err error
	for range 20 {
		info, err = c.files.Info(ctx, id, nil)
		if err == nil && info.IsReady {
			return info, nil
		}
		time.Sleep(time.Second)
	}
	return info, err
}

func (c clients) cleanup(ctx context.Context, ledger string) error {
	f, err := os.Open(ledger)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var errs []error
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id := strings.TrimSpace(sc.Text())
		if id == "" || !utf8.ValidString(id) {
			continue
		}
		n++
		if _, err := c.files.Delete(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", id, err))
			continue
		}
		info, err := c.files.Info(ctx, id, nil)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("verify %s: %w", id, err))
		case info.RemovedAt == nil:
			errs = append(errs, fmt.Errorf("verify %s: not removed", id))
		}
	}
	fmt.Printf("\ncleanup: %d uploads deleted and verified removed, %d errors\n", n-len(errs), len(errs))
	return errors.Join(append(errs, sc.Err())...)
}
