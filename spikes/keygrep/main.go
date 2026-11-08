// Command keygrep counts how often a project's public and secret keys
// appear in files, without printing the keys: the check that secrets never
// reach a log. It exits 1 when either key appears.
//
//	go run ./spikes/keygrep -project alex-gusev-test mount.log status.txt
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/alxgsv/ucfs/internal/config"
)

func main() {
	project := flag.String("project", "", "project name from ~/.uploadcare/config.yaml")
	cfgPath := flag.String("config", "", "config file (default ~/.uploadcare/config.yaml)")
	pub := flag.String("public-key", "", "a public key to look for instead of a project's")
	sec := flag.String("secret-key", "", "a secret key to look for instead of a project's")
	flag.Parse()
	cfg, err := config.Load(config.Options{ConfigPath: *cfgPath, Project: *project, PublicKey: *pub, SecretKey: *sec})
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygrep:", err)
		os.Exit(2)
	}
	found := false
	fmt.Printf("%-60s %10s %12s %12s\n", "file", "bytes", "public key", "secret key")
	for _, p := range flag.Args() {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "keygrep:", err)
			os.Exit(2)
		}
		np := bytes.Count(data, []byte(cfg.PublicKey))
		ns := bytes.Count(data, []byte(cfg.SecretKey))
		found = found || np+ns > 0
		fmt.Printf("%-60s %10d %12d %12d\n", p, len(data), np, ns)
	}
	if found {
		os.Exit(1)
	}
}
