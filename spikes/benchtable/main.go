// Command benchtable reads `go test -bench` output on stdin and prints the
// benchmarks as a Markdown table: one row per benchmark, the median of its
// runs when -count repeated it, and a column per unit reported. With -old,
// a second table compares each median with the same benchmark's in an
// earlier run's output.
//
//	go test -run '^$' -bench . ./internal/vfs | go run ./spikes/benchtable
//	go run ./spikes/benchtable -old previous.txt < bench.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var line = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+(.*)$`)

// shown renames units for the table; ns/op becomes milliseconds.
var shown = map[string]string{"ns/op": "ms/op"}

type results struct {
	names, units []string
	values       map[string]map[string][]float64
}

func parse(r io.Reader) results {
	res := results{values: map[string]map[string][]float64{}}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		m := line.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		name := strings.TrimPrefix(m[1], "Benchmark")
		if res.values[name] == nil {
			res.values[name] = map[string][]float64{}
			res.names = append(res.names, name)
		}
		f := strings.Fields(m[2])
		for i := 0; i+1 < len(f); i += 2 {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				continue
			}
			unit := f[i+1]
			if unit == "ns/op" {
				v /= 1e6
			}
			if u, ok := shown[unit]; ok {
				unit = u
			}
			if !slices.Contains(res.units, unit) {
				res.units = append(res.units, unit)
			}
			res.values[name][unit] = append(res.values[name][unit], v)
		}
	}
	return res
}

func (r results) median(name, unit string) (float64, bool) {
	v := slices.Clone(r.values[name][unit])
	if len(v) == 0 {
		return 0, false
	}
	slices.Sort(v)
	return v[len(v)/2], true
}

func header(units []string) {
	fmt.Printf("| benchmark | %s |\n|---|%s\n", strings.Join(units, " | "), strings.Repeat("---:|", len(units)))
}

func main() {
	oldPath := flag.String("old", "", "an earlier run's output to compare with")
	flag.Parse()
	cur := parse(os.Stdin)
	if len(cur.names) == 0 {
		fmt.Fprintln(os.Stderr, "benchtable: no benchmark lines on stdin")
		os.Exit(1)
	}
	header(cur.units)
	for _, n := range cur.names {
		row := []string{n}
		for _, u := range cur.units {
			v, ok := cur.median(n, u)
			if !ok {
				row = append(row, "")
				continue
			}
			row = append(row, strconv.FormatFloat(v, 'f', 1, 64))
		}
		fmt.Printf("| %s |\n", strings.Join(row, " | "))
	}
	if *oldPath == "" {
		return
	}
	f, err := os.Open(*oldPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchtable:", err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()
	prev := parse(f)
	fmt.Printf("\nChange from the previous run (medians, previous → now):\n\n")
	header(cur.units)
	for _, n := range cur.names {
		row := []string{n}
		for _, u := range cur.units {
			now, ok := cur.median(n, u)
			was, had := prev.median(n, u)
			switch {
			case !ok:
				row = append(row, "")
			case !had:
				row = append(row, "new")
			default:
				row = append(row, fmt.Sprintf("%.1f → %.1f (%+.0f%%)", was, now, 100*(now-was)/max(was, 1e-9)))
			}
		}
		fmt.Printf("| %s |\n", strings.Join(row, " | "))
	}
}
