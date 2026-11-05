// Command scenario creates and checks scenario packs, for `make scenario` and
// `make scenario-check`:
//
//	scenario new <name>        create scenarios/<name> to fill in
//	scenario check [<name>...] check every pack, or those named
//	scenario judge             decide the proposals on stdin, for the evaluation
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hzerrad/themis/eval"
)

const root = "scenarios"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "new":
		if len(os.Args) != 3 {
			usage()
		}
		name := os.Args[2]
		if err := eval.Scaffold(root, name); err != nil {
			fmt.Fprintln(os.Stderr, "scenario:", err)
			os.Exit(1)
		}
		fmt.Printf("Created %s/%s. Fill in every TODO, then run `make scenario-check NAME=%s`.\n", root, name, name)
	case "check":
		if !check(os.Args[2:]) {
			os.Exit(1)
		}
	case "judge":
		if err := judge(); err != nil {
			fmt.Fprintln(os.Stderr, "scenario:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: scenario new <name> | scenario check [<name>...] | scenario judge")
	os.Exit(2)
}

// check checks the packs named, or every pack, and says what each lacks.
func check(names []string) bool {
	if len(names) == 0 {
		entries, err := os.ReadDir(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "scenario:", err)
			return false
		}
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	ok := true
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.IsDir() { //nolint:gosec // a pack the contributor named
			fmt.Printf("%s: no scenario pack has this name in %s/\n", name, root)
			ok = false
			continue
		}
		r, err := eval.CheckPack(context.Background(), root, name, false)
		if err != nil {
			ok = false
			fmt.Printf("%s: not ready\n", name)
			for line := range strings.SplitSeq(err.Error(), "\n") {
				fmt.Printf("  - %s\n", line)
			}
			continue
		}
		fmt.Printf("%s: ok, %d cases: %d where acting is right, %d look-alikes\n", name, r.Cases, r.Act, r.LookAlikes)
	}
	return ok
}

// judge reads a JSON list of proposals and writes Themis's verdict on each,
// in order.
func judge() error {
	var proposals []eval.Proposal
	if err := json.NewDecoder(os.Stdin).Decode(&proposals); err != nil {
		return fmt.Errorf("proposals: %w", err)
	}
	verdicts, err := eval.Judge(context.Background(), root, proposals)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(verdicts)
}
