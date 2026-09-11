# QuickADBackup

Incremental backup of an Android device's shared storage to a PC, speaking the
ADB protocol directly over USB. **No adb.exe, no adb server, no cgo.**

`adb pull -a /sdcard` can only ever do a full backup: it has nothing to compare
against, so every run re-transfers everything. This tool reads the device's file
metadata first, works out what actually changed, and fetches only that.

**The device is strictly read-only.** Nothing is created, modified or deleted on
the phone. Files that disappear from the phone are moved into a dated archive
folder on the PC rather than deleted.

## Usage

There are two front ends over one engine.

**`quickadbackup-gui.exe`** is a native Windows window: pick a destination,
watch a progress bar and transfer rate, cancel mid-run. It remembers the
destination between sessions and mirrors its log to
`%APPDATA%\QuickADBackup\gui.log`, so a completed backup leaves a record.

The GUI links the engine packages directly rather than driving the CLI. That is
not a style preference — the USB interface can be claimed by exactly one
process, so a GUI that spawned the CLI would be fighting its own child for the
device. Linking in also lets one connection stay open for the life of the
window, which means the GUI pays the interface-reappearance wait once at
startup instead of on every operation.

**`quickadbackup.exe`** is the command line, for scripting and scheduled runs:

```
quickadbackup probe                          # check what the device supports
quickadbackup sync -dest D:\Backup           # copy new and changed files
quickadbackup verify -dest D:\Backup         # re-hash the backup against the phone
```

Only one of the two can hold the device at a time.

| Flag | Meaning |
| --- | --- |
| `-dest DIR` | destination on this PC (required) |
| `-root DIR` | device folder to back up (default `/storage/emulated/0`) |
| `-exclude LIST` | comma-separated paths to skip, relative to root |
| `-workers N` | parallel transfer streams (default 8) |
| `-dry-run` | report the plan, copy nothing |
| `-sample N` | `verify` only: files to check at random; `0` checks every file |
| `-device FRAG` | which device to use, when more than one is connected |

Running under Git Bash or MSYS, set `MSYS_NO_PATHCONV=1` first, or the shell
rewrites `/storage/...` into a Windows path before the tool ever sees it.

`sync` and `verify` exit non-zero whenever the run did not establish what it set
out to establish, not only when it crashed. A backup that could not read some of
the files it wanted, or a verify that could not hash some of them on the device,
is a failure: a scheduled run only ever looks at the exit status, and the one
number it reads must not say "clean" about files nobody compared.

### The one real cost of not using adb

The USB interface can be claimed by exactly one process. **The adb server must
not be running**, and Android Studio or scrcpy cannot be connected at the same
time.

There is a second consequence. When a host disconnects, `adbd` on the phone
tears its USB function down and brings it back up, so the interface disappears
from Windows entirely for about 3.6 seconds. Running this tool twice in quick
succession waits that out; it prints a line when it does.

## How it works

Three layers, each in its own package:

| Layer | Package | What it does |
| --- | --- | --- |
| USB | `internal/winusb` | Claims the ADB interface through Microsoft's WinUSB driver via SetupAPI, and moves bytes over the bulk endpoints |
| Protocol | `internal/adbproto` | CNXN/AUTH/OPEN/OKAY/WRTE/CLSE framing, RSA authentication, stream multiplexing |
| Sync | `internal/adbproto/sync.go` | The file service: `STAT_V2`, `LIST_V2`, `RECV` |

None of this is reverse engineered. It is documented in AOSP under
`packages/modules/adb`: `protocol.txt` for the framing, `SERVICES.TXT` for the
service names, `SYNC.TXT` for file transfer.

Authentication reuses `~/.android/adbkey`, the same key adb uses, so a device
that has already authorized this computer stays authorized. If that key does not
exist - a PC that has never run adb - one is generated, in adb's own formats:
PKCS#8 PEM for the private half, and Android's 524-byte little-endian
`RSAPublicKey` struct, base64-encoded, for `adbkey.pub`. A key written here is
therefore interchangeable with adb's own. (The encoding is checked against
reality: the test suite re-derives the public key from a private key adb itself
wrote and compares the result byte for byte.)

Setting `ANDROID_VENDOR_KEYS` points the lookup elsewhere and disables
generation, since a vendor key directory is not this tool's to write into.

The first authorization completes in one run. The device sends a token, the host
signs it, the device rejects the unknown key and sends another, the host offers
the public half, and the phone raises "Allow USB debugging?". The tool then keeps
signing tokens until the connection completes or a minute passes - it no longer
gives up at the prompt and asks you to run it again.

**Scanning** runs one shell command rather than walking the sync service:

```
find /storage/emulated/0 -type f -printf '%s|%T@|%p\n'
```

This is deliberate. The sync service's own `LIST` costs one round trip per
directory: across 7,959 directories that measured 29.1 s, against 2.9 s for a
single `find`. Purity would have made it ten times slower.

**Transfer** uses the sync service's `RECV` on a pool of parallel streams. There
is no size threshold, no `tar`, and no batching by command length, because none
of those constraints exist once the shell is out of the transfer path.

## Measurements

Pixel 7, Android 16, USB 3.

| | adb-based build | This build |
| --- | --- | --- |
| First backup of `Pictures` (7,204 files / 1.1 GiB) | 49.3 s | **18.0 s** |
| Same backup, nothing changed | 1.8 s | 1.8 s |
| Same cold workload, transfer only | 15.3 MiB/s | **101.7 MiB/s** |
| Full verify of 7,204 files | — | 38.8 s, all matched |

Transfer scaling with `-workers`, measured on cold files:

| Streams | Throughput |
| --- | --- |
| 1 | 117 files/s |
| 4 | 489 files/s |
| 8 | 817 files/s, 75 MiB/s |

Throughput varies with what the phone is doing. One 285 MiB run took 4m55s that
normally takes 27 s, apparently because the device had gone into a doze state.

## Device quirks this works around

These were all found by testing against a real device, and each one silently
produces a wrong result rather than an error.

**`find /sdcard` returns nothing.** `/sdcard` is a symlink to
`/storage/self/primary`, itself a symlink to `/storage/emulated/0`, and `find`
does not descend into a symlinked starting point. It exits successfully having
listed one entry. The tool uses the real path.

**`find -exec stat -c ... {} +` truncates silently.** toybox aborts the batch
with `Argument list too long` — sent to stderr, where it is usually discarded —
and returns a partial listing with a zero exit status. It returned 3,064 of
25,761 files. Every listing is cross-checked against a plain `find | wc -l` and
the run aborts if the counts disagree.

**The `shell` service corrupts binary data.** It allocates a pseudo-terminal
that expands LF to CRLF: a 4,869,012-byte JPEG came back as 4,876,491 bytes with
a different SHA-1. All commands go through the `exec` service instead, which
allocates no terminal.

**`tar` skips files it cannot read, without failing.** This is why the transfer
path uses `RECV`, which returns an explicit `FAIL` for the one file it could not
read instead of quietly omitting it from a batch.

**A reconnect reads the previous session's tail.** Data left in the USB pipes
survives a close, so the next connection's first header lands mid-stream. Both
pipes are reset and flushed on open, and every header's magic word is checked.

**Windows filenames are more restrictive than Android's.** Names containing
`: ? * " < > |`, names ending in a dot or space, and reserved names like `CON`
are percent-encoded. Files whose names differ only by case cannot coexist on
NTFS, so those are skipped and reported rather than silently overwriting each
other.

**`sha1sum` may separate the digest from the path with `*`, not a space.** It
marks binary mode, and it is not whitespace. Trimming the separator as if it were
left the asterisk glued to the path, which then matched no file the tool had
asked about, so every result in the batch was filed as "the device could not read
this" - and an unreadable file used not to fail the run.

**`'''` is not how you escape an apostrophe for `sh`.** It leaves the string
unterminated; the correct form closes the quoted run and reopens it, `'\''`. One
file called `Mom's birthday.jpg` was enough to make an entire `sha1sum` batch
unparseable.

**The `exec` service carries no exit status.** A command that fails to parse is
indistinguishable from one that succeeded with no output, which is how the
quoting bug above stayed invisible. Every command now runs inside a compound that
prints its own exit status, and a missing status marker is an error in itself.

**A directory the shell user cannot open vanishes from the listing and from the
count it is checked against.** The two agree, every number in the summary adds up,
and everything underneath is missing from the backup. `find` does report it, on
stderr, and its exit status says so; both are now acted on.

**`Android/data` and `Android/obb` are excluded by default.** They are app-
private scoped storage, unreadable to the adb shell user on Android 11+, and
hold app caches rather than user data — 12,960 files on the test device.

## Building

```
go build -o quickadbackup.exe .
go build -ldflags "-H=windowsgui" -o quickadbackup-gui.exe ./cmd/gui
```

`cmd/gui/rsrc.syso` embeds `app.manifest`, which the GUI cannot run without:
walk needs Common Controls 6.0, and lacking it every widget fails at
`TTM_ADDTOOL` and the window silently never opens. Regenerate it after editing
the manifest:

```
go run github.com/akavel/rsrc@latest -manifest cmd/gui/app.manifest -arch amd64 -o cmd/gui/rsrc.syso
```

## One instance at a time

Both binaries take a named mutex at startup and refuse to run if the other
already holds it. Without it a second copy sat waiting 20 seconds for a device
it was never going to get; now it says so in 50 ms.

The same distinction runs deeper: `winusb.ErrInUse` (another process holds the
interface) is not retried, while the interface briefly vanishing during adbd's
re-enumeration is. Only one of those is worth waiting out. That sentence was
aspirational for a while - `Open` wrapped the raw Windows errno and never the
sentinel, so the check that acts on it could never match and a running adb server
cost the full twenty-second retry loop every time.

More than one device connected is a third case, and it is neither retried nor
guessed at. Picking the first interface Windows enumerates makes the answer to
"which phone did that back up" depend on enumeration order; the CLI asks for
`-device` with enough of a path to tell them apart, and the GUI asks you to
unplug the others.

## GUI threading rules

walk requires every window call on the UI thread, and operations run on
background goroutines, so both directions need marshalling. Each rule below
exists because breaking it produced a visible bug:

- Progress arrives once per file, far faster than a window repaints. Only the
  newest update is kept and at most one repaint is ever queued.
- **The progress area is reset when an operation ends**, not left where it
  stopped. A cancelled verify used to leave the bar at 71% and the label
  reading "検証中 5170 / 7204" — a stopped operation that looked like a running
  one.
- **The teardown runs before any dialog.** A modal runs its own message loop,
  so anything queued behind it waits for a click. Showing the dialog first left
  the window advertising an operation that had already finished.
- Dialogs raised from a worker go through `Synchronize`. A modal shown directly
  from a worker parks that goroutine until someone clicks.
- Settings are read on the window's `Closing` event, not after `Run` returns.
  Reading a widget afterwards touches a destroyed HWND and hung the process
  with no window on screen.
- Closing the device is bounded by a timeout, so a reader parked on a USB
  transfer cannot keep a windowless process alive.

### Phases with nothing to count

Scanning the device takes 26 s on the test phone and reports no counts until it
finishes, so the bar runs in marquee mode until a phase produces a real total.
A motionless window for half a minute reads as a hang.

Verifying has the same problem in its first half: hashing the local copies is
pure disk work with no device traffic. It now reports its own progress and
checks for cancellation — before that, pressing 中止 during it did nothing
until the whole phase finished.

## Interruption

The index is written every few seconds, so an interrupted run resumes from where
it stopped instead of starting over. Files are written to a `.part` file, flushed
to disk and renamed, so a partial file is never mistaken for a complete one and a
power loss cannot leave a full-length file of zeroes that the index believes is
done. The next run sweeps away any `.part` left behind, except one whose name the
device actually has.

The first Ctrl+C cancels the run so it can unwind and still print its summary; a
second one quits immediately. Cancelling works even when the phone has stopped
answering: a USB bulk read has no timeout of its own, so a cancellation that
produces no progress for ten seconds tears the connection down rather than
leaving every worker parked in the driver forever.
