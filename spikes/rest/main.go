// Command rest sends one REST API request with the credentials of a
// configured project and prints the status and the response body, for
// measurements the uploadcare CLI has no command for (tags, search, raw
// file info). It exits 1 on a status of 300 or more.
//
//	go run ./spikes/rest -project alex-gusev-test GET /files/<uuid>/
//	go run ./spikes/rest -project alex-gusev-test PUT /files/<uuid>/tags/ '{"tags":["a"]}'
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/uploadcare/uploadcare-go/v2/ucare"

	"github.com/alxgsv/ucfs/internal/config"
)

type jsonBody []byte

func (b jsonBody) EncodeReq(r *http.Request) error {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Type", "application/json")
	return nil
}

func main() {
	project := flag.String("project", "", "project name from ~/.uploadcare/config.yaml")
	status := flag.Bool("status", false, "print the HTTP status before the body")
	flag.Parse()
	if flag.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: rest [-project name] METHOD PATH [JSON]")
		os.Exit(2)
	}
	cfg, err := config.Load(config.Options{Project: *project})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	creds := ucare.APICreds{PublicKey: cfg.PublicKey, SecretKey: cfg.SecretKey}
	ucfg, err := ucare.NewConfig(creds, ucare.WithUserAgent("ucfs-spike"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	uc, err := ucare.NewClient(creds, ucfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var data ucare.ReqEncoder
	if flag.NArg() > 2 {
		data = jsonBody(flag.Arg(2))
	}
	req, err := uc.NewRequest(context.Background(), "api.uploadcare.com", flag.Arg(0), flag.Arg(1), data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *status {
		fmt.Printf("HTTP %d\n", resp.StatusCode)
	}
	fmt.Println(string(body))
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}
