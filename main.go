// Command quickadbackup incrementally mirrors an Android device's shared
// storage to this PC.
//
// It talks the ADB protocol straight to the phone over WinUSB. There is no
// adb.exe and no adb server, which also means the USB interface is claimed
// exclusively: adb, Android Studio and scrcpy cannot be using it at the same
// time.
//
// The device is treated as strictly read-only. Nothing is ever written to or
// deleted from the phone. Files that disappear from the phone are moved into a
// dated archive folder on the PC rather than deleted.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"quickadbackup/internal/adbproto"
	"quickadbackup/internal/device"
	"quickadbackup/internal/probe"
	"quickadbackup/internal/progress"
	"quickadbackup/internal/singleton"
	"quickadbackup/internal/sync"
	"quickadbackup/internal/verify"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `quickadbackup - incremental Android backup, without adb

usage:
  quickadbackup probe [-root DIR]              check what the device supports
  quickadbackup sync -dest DIR [options]       copy new and changed files
  quickadbackup verify -dest DIR [-sample N]   re-hash the backup against the phone

sync options:
  -dest DIR       destination folder on this PC (required)
  -root DIR       device folder to back up (default `+device.DefaultRoot+`)
  -exclude LIST   comma-separated paths to skip, relative to root
  -workers N      parallel transfer streams (default 8)
  -dry-run        report what would be copied, copy nothing
  -device FRAG    which device to use, when more than one is connected

`)
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("no command given")
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		usage()
		return nil
	}

	// The GUI and the command line both claim the phone exclusively, so only
	// one of them may run at a time.
	lock, err := singleton.Acquire()
	if err != nil {
		return fmt.Errorf("%w\n\n"+
			"QuickADBackup (GUI か別のコマンド) がすでに実行中です。"+
			"スマホのUSBインターフェースは1プロセスしか掴めません", err)
	}
	defer lock.Release()

	// The first Ctrl+C cancels the run so it can unwind and still print its
	// summary; the second one takes the process down. signal.NotifyContext on
	// its own swallows every interrupt after the first, so a transfer wedged on
	// a USB read left no way out but the task manager.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		cancel()
		fmt.Fprintln(os.Stderr, "\nstopping - press Ctrl+C again to quit immediately")
		<-sigs
		os.Exit(130)
	}()

	switch os.Args[1] {
	case "probe":
		return cmdProbe(ctx, os.Args[2:])
	case "sync":
		return cmdSync(ctx, os.Args[2:])
	case "verify":
		return cmdVerify(ctx, os.Args[2:])
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

// connect claims the device, explaining the wait if adbd is still re-exposing
// its USB interface after a previous run.
func connect(match string) (*adbproto.Conn, error) {
	c, err := adbproto.DialTimeoutSelect(20*time.Second, match, func() {
		fmt.Println("waiting for the device's USB interface ...")
	})
	if err != nil {
		return nil, fmt.Errorf("%w\n\n"+
			"checklist:\n"+
			"  - the phone is plugged in and unlocked, with USB debugging on\n"+
			"  - no adb server is running (this tool claims the USB interface itself)\n"+
			"  - Android Studio and scrcpy are closed", err)
	}
	return c, nil
}

func cmdProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	root := fs.String("root", device.DefaultRoot, "device directory to inspect")
	dev := fs.String("device", "", "which device to use, when more than one is connected")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect(*dev)
	if err != nil {
		return err
	}
	defer c.Close()

	rep, err := probe.Run(ctx, c, *root)
	if err != nil {
		return err
	}
	rep.Print(os.Stdout)
	return nil
}

func cmdSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dest := fs.String("dest", "", "destination folder on this PC")
	root := fs.String("root", device.DefaultRoot, "device directory to back up")
	exclude := fs.String("exclude", strings.Join(device.DefaultExcludes, ","), "paths to skip")
	workers := fs.Int("workers", sync.DefaultWorkers, "parallel transfer streams")
	dryRun := fs.Bool("dry-run", false, "report the plan without copying")
	dev := fs.String("device", "", "which device to use, when more than one is connected")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dest == "" {
		return fmt.Errorf("-dest is required")
	}

	c, err := connect(*dev)
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Printf("device: %s\ndest:   %s\n\n", deviceName(c.Banner), *dest)

	var excludes []string
	for _, e := range strings.Split(*exclude, ",") {
		if e = strings.TrimSpace(e); e != "" {
			excludes = append(excludes, e)
		}
	}

	st, err := sync.Run(ctx, c, sync.Options{
		Dest:     *dest,
		Root:     *root,
		Excludes: excludes,
		Workers:  *workers,
		DryRun:   *dryRun,
		Progress: cliProgress(),
	})
	if st != nil {
		printSummary(st, *dryRun)
	}
	if err != nil {
		return err
	}
	// A run that could not fetch part of what it set out to fetch must not exit
	// zero. The shortfall is already printed, but a scheduled backup only ever
	// looks at the exit status.
	if st != nil && !*dryRun && len(st.Failed) > 0 {
		return fmt.Errorf("%d of %d files could not be retrieved from the device",
			len(st.Failed), len(st.Failed)+st.Copied)
	}
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dest := fs.String("dest", "", "backup folder to check")
	root := fs.String("root", device.DefaultRoot, "device directory the backup came from")
	exclude := fs.String("exclude", strings.Join(device.DefaultExcludes, ","), "paths to skip")
	sample := fs.Int("sample", 200, "files to check at random; 0 checks every file")
	dev := fs.String("device", "", "which device to use, when more than one is connected")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dest == "" {
		return fmt.Errorf("-dest is required")
	}
	c, err := connect(*dev)
	if err != nil {
		return err
	}
	defer c.Close()

	var excludes []string
	for _, e := range strings.Split(*exclude, ",") {
		if e = strings.TrimSpace(e); e != "" {
			excludes = append(excludes, e)
		}
	}
	res, err := verify.Run(ctx, c, verify.Options{
		Dest: *dest, Root: *root, Excludes: excludes, Sample: *sample,
		Progress: cliProgress(),
	})
	if res != nil {
		fmt.Printf("\n%d checked, %d matched, %d differing, %d missing, %d unreadable, in %s\n",
			res.Checked, res.Matched, len(res.Mismatch), len(res.Missing), len(res.Unreadable),
			res.Elapsed.Round(100_000_000))
		report := func(label string, list []string) {
			for i, f := range list {
				if i == 10 {
					fmt.Fprintf(os.Stderr, "  ... and %d more %s\n", len(list)-10, label)
					break
				}
				fmt.Fprintf(os.Stderr, "  %s: %s\n", label, f)
			}
		}
		report("differs", res.Mismatch)
		report("missing", res.Missing)
		report("unreadable", res.Unreadable)

		// An unreadable file is not a pass. The device returned no hash for it,
		// so the run established nothing about it either way, and a verify that
		// exits 0 while having compared nothing is the worst thing a backup
		// tool can do: a broken quoting helper once made whole sha1sum batches
		// unparseable and the run still reported a clean bill of health.
		//
		// A run that failed outright reports its own error below; this is only
		// about a run that completed.
		considered := res.Checked + len(res.Missing)
		switch {
		case err != nil:
		case res.Checked == 0 && considered > 0:
			return fmt.Errorf("backup not confirmed: none of the %d files on the device could be checked", considered)
		case len(res.Mismatch) > 0 || len(res.Missing) > 0 || len(res.Unreadable) > 0:
			return fmt.Errorf("backup not confirmed: %d files differ, %d are missing from the backup, "+
				"and %d could not be hashed on the device; only the %d files reported as matched are known good",
				len(res.Mismatch), len(res.Missing), len(res.Unreadable), res.Matched)
		}
	}
	return err
}

// cliProgress prints message updates verbatim and thins the per-file stream
// down to something a terminal can keep up with.
func cliProgress() progress.Func {
	last := 0
	return func(u progress.Update) {
		if u.Message != "" {
			fmt.Println(u.Message)
			return
		}
		if u.Total > 0 && (u.Done-last >= 500 || u.Done == u.Total) {
			last = u.Done
			fmt.Printf("  %d/%d files (%s)\n", u.Done, u.Total, progress.HumanBytes(u.Bytes))
		}
	}
}

func deviceName(banner string) string {
	for _, f := range strings.Split(strings.TrimPrefix(banner, "device::"), ";") {
		if v, ok := strings.CutPrefix(f, "ro.product.model="); ok {
			return v
		}
	}
	return "unknown device"
}

func printSummary(st *sync.Stats, dryRun bool) {
	verb := "copied"
	if dryRun {
		verb = "would copy"
	}
	fmt.Printf("\n%s %d files (%s), %d unchanged, %d archived",
		verb, st.Copied, humanBytes(st.CopiedBytes), st.Unchanged, st.Archived)
	if st.Elapsed > 0 {
		fmt.Printf(", in %s", st.Elapsed.Round(100_000_000))
	}
	fmt.Println()

	for _, s := range st.Skipped {
		fmt.Fprintf(os.Stderr, "skipped: %s\n", s)
	}
	if len(st.Failed) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d files could not be retrieved:\n", len(st.Failed))
		for i, f := range st.Failed {
			if i == 10 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(st.Failed)-10)
				break
			}
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
