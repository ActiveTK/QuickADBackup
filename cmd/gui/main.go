// Command quickadbackup-gui is a desktop front end for the backup engine.
//
// It links the engine packages directly rather than driving the CLI. That is
// not a style preference: the USB interface can be claimed by exactly one
// process, so a GUI that spawned the CLI would be fighting its own child for
// the device.
//
// Linking in also lets one connection stay open for the life of the window.
// Each CLI run pays about 3.6 s while adbd re-exposes its USB interface after
// the previous disconnect; here that is paid once at startup.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"quickadbackup/internal/adbproto"
	"quickadbackup/internal/device"
	"quickadbackup/internal/progress"
	"quickadbackup/internal/singleton"
	syncengine "quickadbackup/internal/sync"
	"quickadbackup/internal/verify"
)

type app struct {
	mw *walk.MainWindow

	deviceLabel  *walk.Label
	destEdit     *walk.LineEdit
	rootEdit     *walk.LineEdit
	progressBar  *walk.ProgressBar
	statusLabel  *walk.Label
	currentLabel *walk.Label
	logBox       *walk.TextEdit

	browseBtn  *walk.PushButton
	scanBtn    *walk.PushButton
	syncBtn    *walk.PushButton
	verifyBtn  *walk.PushButton
	cancelBtn  *walk.PushButton
	connectBtn *walk.PushButton

	mu      sync.Mutex
	conn    *adbproto.Conn
	cancel  context.CancelFunc
	running bool

	// inflight counts operation goroutines, so shutdown can give a running one
	// a moment to unwind instead of closing the connection underneath it.
	inflight sync.WaitGroup
	// closing is set once the window is on its way out. After that the UI
	// thread will never run another queued function, so nothing may be
	// marshalled onto it.
	closing atomic.Bool

	// Progress arrives once per file, which is far faster than a window can
	// repaint. The newest update is kept and only one repaint is ever queued.
	latest    atomic.Pointer[progress.Update]
	repaintQd atomic.Bool
	opStart   time.Time
	marquee   bool // UI thread only
}

func main() {
	// Only one process can hold the phone, so a second instance could never do
	// anything except sit waiting for a device it will never get.
	lock, err := singleton.Acquire()
	if err != nil {
		walk.MsgBox(nil, "QuickADBackup",
			"QuickADBackup はすでに起動しています。\n\n"+
				"スマホのUSBインターフェースは1つのプロセスしか掴めないため、\n"+
				"2つ目を起動しても接続できません。既存のウィンドウを使ってください。",
			walk.MsgBoxIconWarning)
		os.Exit(1)
	}
	defer lock.Release()

	a := &app{}
	if err := a.build(); err != nil {
		walk.MsgBox(nil, "QuickADBackup", "起動に失敗しました:\n"+err.Error(),
			walk.MsgBoxIconError)
		os.Exit(1)
	}
	s := loadSettings()
	a.destEdit.SetText(s.Dest)
	if s.Root != "" {
		a.rootEdit.SetText(s.Root)
	}

	// Settings are captured while the window is still alive. Reading a widget
	// after Run returns means touching a destroyed HWND.
	//
	// Closing during an operation used to do neither of the two things it
	// should: the goroutine kept copying while shutdown pulled the connection
	// out from under it, and it left .part files behind. Ask first, then cancel
	// so the engine can stop at a file boundary and flush its index.
	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		saveSettings(settings{Dest: a.destEdit.Text(), Root: a.rootEdit.Text()})

		if a.isRunning() {
			if walk.MsgBox(a.mw, "QuickADBackup",
				"バックアップを実行中です。中止して終了しますか?",
				walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
				*canceled = true
				return
			}
		}
		a.closing.Store(true)
		a.cancelOp()
	})

	go a.connect()
	a.mw.Run()

	a.shutdown()
}

// shutdown releases the device, but never lets that keep the process alive.
//
// Closing waits for the reader goroutine to return, and a reader parked on a
// USB transfer that refuses to cancel would otherwise leave a windowless
// process running forever. The OS reclaims the handle either way.
//
// A cancelled operation is given the same bounded grace first, so it can finish
// unwinding and flush its index rather than be shot in the head mid-write.
func (a *app) shutdown() {
	ops := make(chan struct{})
	go func() { a.inflight.Wait(); close(ops) }()
	select {
	case <-ops:
	case <-time.After(5 * time.Second):
		appendLog(time.Now().Format("15:04:05") + "  実行中の処理が時間内に終了しませんでした。プロセスを終了します。")
	}

	a.mu.Lock()
	c := a.conn
	a.conn = nil
	a.mu.Unlock()
	if c == nil {
		return
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		appendLog(time.Now().Format("15:04:05") + "  USB接続の切断が完了しませんでした。プロセスを終了します。")
	}
}

func (a *app) build() error {
	return MainWindow{
		AssignTo: &a.mw,
		Title:    "QuickADBackup",
		MinSize:  Size{Width: 720, Height: 520},
		Size:     Size{Width: 860, Height: 600},
		Layout:   VBox{MarginsZero: false},
		Children: []Widget{
			GroupBox{
				Title:  "デバイス",
				Layout: HBox{},
				Children: []Widget{
					Label{AssignTo: &a.deviceLabel, Text: "接続していません"},
					HSpacer{},
					PushButton{AssignTo: &a.connectBtn, Text: "再接続", MaxSize: Size{Width: 100},
						OnClicked: func() { go a.connect() }},
				},
			},
			GroupBox{
				Title:  "設定",
				Layout: Grid{Columns: 3},
				Children: []Widget{
					Label{Text: "バックアップ先:"},
					LineEdit{AssignTo: &a.destEdit},
					PushButton{AssignTo: &a.browseBtn, Text: "参照...", MaxSize: Size{Width: 100},
						OnClicked: a.browseDest},

					Label{Text: "デバイス側の対象:"},
					LineEdit{AssignTo: &a.rootEdit, Text: device.DefaultRoot},
					Label{Text: ""},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{AssignTo: &a.scanBtn, Text: "スキャン (差分を確認)",
						OnClicked: func() { go a.runSync(true) }},
					PushButton{AssignTo: &a.syncBtn, Text: "バックアップ",
						OnClicked: func() { go a.runSync(false) }},
					PushButton{AssignTo: &a.verifyBtn, Text: "検証",
						OnClicked: func() { go a.runVerify() }},
					HSpacer{},
					PushButton{AssignTo: &a.cancelBtn, Text: "中止", MaxSize: Size{Width: 100},
						OnClicked: a.cancelOp},
				},
			},
			GroupBox{
				Title:  "進捗",
				Layout: VBox{},
				Children: []Widget{
					ProgressBar{AssignTo: &a.progressBar, MaxValue: 1000},
					Label{AssignTo: &a.statusLabel, Text: "待機中"},
					Label{AssignTo: &a.currentLabel, Text: " "},
				},
			},
			GroupBox{
				Title:  "ログ",
				Layout: VBox{},
				Children: []Widget{
					TextEdit{AssignTo: &a.logBox, ReadOnly: true, VScroll: true},
				},
			},
		},
	}.Create()
}

// msgBox shows a dialog from a worker goroutine.
//
// walk requires window calls on the UI thread, and a modal shown directly from
// a worker also parks that goroutine until someone clicks, which would leave
// the buttons disabled for as long as the dialog stands. Synchronize queues the
// dialog and returns at once, so the operation can finish tidying up.
func (a *app) msgBox(title, message string, style walk.MsgBoxStyle) {
	a.ui(func() { walk.MsgBox(a.mw, title, message, style) })
}

// ui marshals a window update onto the UI thread, and drops it once the window
// is closing.
//
// An operation cancelled by the close keeps running for a moment after Run has
// returned, and by then the HWND is gone: the queued function would never run,
// and a queued modal would be worse than useless.
func (a *app) ui(f func()) {
	if a.closing.Load() {
		return
	}
	a.mw.Synchronize(f)
}

// log appends a line to the window and to the session log file, marshalling the
// window update onto the UI thread. The file copy keeps working after the
// window has gone, which is where the tail of a cancelled operation lands.
func (a *app) log(format string, args ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	appendLog(line)
	a.ui(func() { a.logBox.AppendText(line + "\r\n") })
}

func (a *app) connect() {
	a.ui(func() {
		a.deviceLabel.SetText("接続中...")
		a.connectBtn.SetEnabled(false)
	})

	a.mu.Lock()
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
	a.mu.Unlock()

	a.log("デバイスに接続しています...")
	c, err := adbproto.DialTimeout(20*time.Second, func() {
		a.log("USBインターフェースの再出現を待っています (切断後、adbdが再初期化するため数秒かかります)")
	})
	if err != nil {
		a.log("接続失敗: %v", err)
		a.ui(func() {
			a.deviceLabel.SetText("未接続")
			a.connectBtn.SetEnabled(true)
			a.refreshControls()
		})
		// Several devices at once is not a fault to work through a checklist; it
		// is a question only the user can answer, and the wrong answer would
		// silently back up the wrong phone.
		if errors.Is(err, adbproto.ErrAmbiguousDevice) {
			a.msgBox("どのデバイスを使うか判断できません",
				"Androidデバイスが複数接続されています。\n\n"+
					"このツールは一度に1台だけを扱い、どれを使うかを推測しません。\n"+
					"バックアップしたい1台だけを残して、他を取り外してから「再接続」を押してください。\n\n"+
					err.Error(),
				walk.MsgBoxIconWarning)
			return
		}
		a.msgBox("接続できません",
			err.Error()+"\n\n確認してください:\n"+
				"  ・スマホが接続され、画面ロックが解除されている\n"+
				"  ・USBデバッグが有効になっている\n"+
				"  ・adbサーバーが起動していない (このツールがUSBを占有します)\n"+
				"  ・Android Studio や scrcpy が閉じている",
			walk.MsgBoxIconWarning)
		return
	}

	a.mu.Lock()
	a.conn = c
	a.mu.Unlock()

	name := deviceName(c.Banner)
	a.log("接続しました: %s", name)
	a.ui(func() {
		a.deviceLabel.SetText(name + " (接続済み)")
		a.connectBtn.SetEnabled(true)
		a.refreshControls()
	})
}

func (a *app) browseDest() {
	dlg := new(walk.FileDialog)
	dlg.Title = "バックアップ先フォルダを選択"
	dlg.FilePath = a.destEdit.Text()
	if ok, err := dlg.ShowBrowseFolder(a.mw); err != nil || !ok {
		return
	}
	a.destEdit.SetText(dlg.FilePath)
}

// isRunning reports whether an operation currently holds the slot.
func (a *app) isRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// refreshControls matches the buttons to the current state. It must be called
// on the UI thread.
//
// It only reads a.running: begin and end own that flag and set it
// synchronously, because a widget toggle queued onto the UI thread lands after
// the next click has already been dispatched.
func (a *app) refreshControls() {
	a.mu.Lock()
	running := a.running
	connected := a.conn != nil
	a.mu.Unlock()

	ready := connected && !running
	a.scanBtn.SetEnabled(ready)
	a.syncBtn.SetEnabled(ready)
	a.verifyBtn.SetEnabled(ready)
	a.browseBtn.SetEnabled(!running)
	a.cancelBtn.SetEnabled(running)
	a.connectBtn.SetEnabled(!running)
}

func (a *app) cancelOp() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		a.log("中止しています...")
		cancel()
	}
}

// begin validates preconditions and claims the single operation slot.
//
// The slot is claimed here, under the lock, and not by the widget toggle queued
// below. Every operation starts on a worker goroutine, so a flag set from a
// Synchronize'd function is set long after the click that follows the first one
// has already been dispatched: two quick clicks both got past this check and
// ran two syncs at once over the same connection and into the same folder, with
// only the second one still cancellable.
func (a *app) begin() (*adbproto.Conn, context.Context, bool) {
	a.mu.Lock()
	if a.running || a.conn == nil {
		a.mu.Unlock()
		return nil, nil, false
	}
	c := a.conn
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.running = true
	a.mu.Unlock()

	a.inflight.Add(1)
	a.opStart = time.Now()
	a.ui(func() {
		a.refreshControls()
		a.progressBar.SetValue(0)
		a.currentLabel.SetText(" ")
	})
	return c, ctx, true
}

// outcome is how an operation ended, which decides what the progress area is
// left showing.
type outcome int

const (
	outcomeFailed outcome = iota
	outcomeCancelled
	outcomeDone
)

// end releases the operation slot and puts the progress area into a terminal
// state.
//
// Call it before showing any dialog. A modal runs its own message loop, so
// anything queued behind it - including this teardown - does not run until
// someone clicks, which would leave the window advertising an operation that
// has already stopped.
//
// Resetting matters: leaving the bar part-filled and the label reading
// "検証中 5170 / 7204" after a cancellation makes a stopped operation look like
// a running one. It also lands after any repaint queued while the operation was
// still going, because Synchronize runs in order and this is deferred last.
func (a *app) end(o outcome) {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	// The slot is released here rather than in the widget update below, for the
	// same reason begin claims it here: the next operation must not have to
	// wait for the UI thread to catch up before it is allowed to start.
	a.running = false
	a.mu.Unlock()

	a.ui(func() {
		a.refreshControls()
		a.setMarquee(false)
		a.currentLabel.SetText(" ")
		switch o {
		case outcomeDone:
			a.progressBar.SetValue(1000)
			a.statusLabel.SetText("完了")
		case outcomeCancelled:
			a.progressBar.SetValue(0)
			a.statusLabel.SetText("中止しました")
		default:
			a.progressBar.SetValue(0)
			a.statusLabel.SetText("エラーで停止しました")
		}
	})
}

// onProgress keeps only the newest update and queues at most one repaint, so a
// fast transfer cannot flood the UI thread.
func (a *app) onProgress(u progress.Update) {
	if u.Message != "" {
		a.log("%s", u.Message)
	}
	a.latest.Store(&u)
	if a.repaintQd.Swap(true) {
		return
	}
	a.ui(func() {
		a.repaintQd.Store(false)
		cur := a.latest.Load()
		if cur == nil {
			return
		}
		// A phase with nothing to count yet - scanning the device is the long
		// one - drives the bar in marquee mode. Without it the window sits
		// motionless for the better part of a minute and looks hung.
		a.setMarquee(cur.Total <= 0)
		if cur.Total > 0 {
			a.progressBar.SetValue(int(cur.Fraction() * 1000))
		}
		a.statusLabel.SetText(a.describe(*cur))
		if cur.Current != "" {
			a.currentLabel.SetText(ellipsize(cur.Current, 110))
		}
	})
}

// setMarquee switches the bar between scrolling and measured. It must run on
// the UI thread, and only acts on a change, since re-arming marquee mode
// restarts the animation.
func (a *app) setMarquee(on bool) {
	if a.marquee == on {
		return
	}
	a.marquee = on
	a.progressBar.SetMarqueeMode(on)
	if !on {
		a.progressBar.SetValue(0)
	}
}

func (a *app) describe(u progress.Update) string {
	elapsed := time.Since(a.opStart).Seconds()
	switch u.Phase {
	case progress.PhaseScanning:
		return "デバイスをスキャンしています... (ファイル数によっては数十秒かかります)"
	case progress.PhaseArchiving:
		return "消えたファイルを退避しています..."
	case progress.PhaseHashingLocal:
		if u.Total <= 0 {
			return "PC側のハッシュを計算しています..."
		}
		return fmt.Sprintf("PC側のハッシュ計算  %d / %d ファイル", u.Done, u.Total)
	case progress.PhaseVerifying:
		if u.Total <= 0 {
			return "デバイス側のハッシュを計算しています..."
		}
		return fmt.Sprintf("検証中  %d / %d ファイル", u.Done, u.Total)
	default:
		if u.Total <= 0 {
			return "準備しています..."
		}
		s := fmt.Sprintf("%d / %d ファイル  %s", u.Done, u.Total, progress.HumanBytes(u.Bytes))
		if elapsed > 0.5 {
			s += fmt.Sprintf("  (%s/s, %.0f files/s)",
				progress.HumanBytes(int64(float64(u.Bytes)/elapsed)),
				float64(u.Done)/elapsed)
		}
		return s
	}
}

// finisher returns a one-shot wrapper around end, so an operation can tear the
// UI down at the right moment and still have a deferred safety net.
func (a *app) finisher() func(outcome) {
	done := false
	return func(o outcome) {
		if done {
			return
		}
		done = true
		a.end(o)
	}
}

func (a *app) runSync(dryRun bool) {
	dest := strings.TrimSpace(a.destEdit.Text())
	if dest == "" {
		a.msgBox("QuickADBackup", "バックアップ先を指定してください。", walk.MsgBoxIconInformation)
		return
	}
	c, ctx, ok := a.begin()
	if !ok {
		return
	}
	defer a.inflight.Done()
	defer a.dropIfDead(c)
	out := outcomeFailed
	finish := a.finisher()
	defer func() { finish(out) }()

	if dryRun {
		a.log("--- スキャン開始 ---")
	} else {
		a.log("--- バックアップ開始 -> %s ---", dest)
	}

	st, err := syncengine.Run(ctx, c, syncengine.Options{
		Dest:     dest,
		Root:     strings.TrimSpace(a.rootEdit.Text()),
		Excludes: device.DefaultExcludes,
		DryRun:   dryRun,
		Progress: a.onProgress,
	})
	if err != nil {
		if ctx.Err() != nil {
			a.log("中止しました")
		} else {
			a.log("エラー: %v", err)
			defer a.msgBox("エラー", err.Error(), walk.MsgBoxIconError)
		}
		if ctx.Err() != nil {
			out = outcomeCancelled
		}
		finish(out)
	} else {
		out = outcomeDone
	}
	if st == nil {
		return
	}
	verb := "コピー"
	if dryRun {
		verb = "コピー予定"
	}
	a.log("%s %d件 (%s)、変更なし %d件、退避 %d件、所要 %s",
		verb, st.Copied, progress.HumanBytes(st.CopiedBytes), st.Unchanged, st.Archived,
		st.Elapsed.Round(100*time.Millisecond))
	for _, s := range st.Skipped {
		a.log("スキップ: %s", s)
	}
	// A backup that could not fetch everything is not a finished backup. Leaving
	// the progress area on 完了 with the shortfall buried in the log is the same
	// mistake the verify side used to make: the one number the user reads says
	// the run was clean while it was not.
	//
	// A run the user stopped is not that, though. Cancelling tears the
	// connection down after a grace period, so whatever was mid-transfer lands
	// in Failed - and answering 中止 with a warning that files could not be read
	// blames the device for doing what it was told.
	if n := len(st.Failed); n > 0 && !dryRun && ctx.Err() == nil {
		for i, f := range st.Failed {
			if i == 10 {
				a.log("  ... 他 %d件", n-10)
				break
			}
			a.log("  取得失敗: %s", f)
		}
		out = outcomeFailed
		finish(out)
		a.msgBox("一部のファイルを取得できませんでした",
			fmt.Sprintf("%d件のファイルをデバイスから読み取れませんでした。\n\n"+
				"コピーできたのは %d件です。読み取れなかったファイルの一覧はログを確認してください。\n"+
				"次回の実行で再試行されます。", n, st.Copied),
			walk.MsgBoxIconWarning)
	}
}

func (a *app) runVerify() {
	dest := strings.TrimSpace(a.destEdit.Text())
	if dest == "" {
		a.msgBox("QuickADBackup", "検証するフォルダを指定してください。", walk.MsgBoxIconInformation)
		return
	}
	c, ctx, ok := a.begin()
	if !ok {
		return
	}
	defer a.inflight.Done()
	defer a.dropIfDead(c)
	out := outcomeFailed
	finish := a.finisher()
	defer func() { finish(out) }()

	a.log("--- 検証開始 (SHA1をデバイス側とPC側で独立に計算) ---")
	res, err := verify.Run(ctx, c, verify.Options{
		Dest:     dest,
		Root:     strings.TrimSpace(a.rootEdit.Text()),
		Excludes: device.DefaultExcludes,
		Sample:   0,
		Progress: a.onProgress,
	})
	if err != nil {
		if ctx.Err() != nil {
			a.log("中止しました")
		} else {
			a.log("エラー: %v", err)
		}
		if ctx.Err() != nil {
			out = outcomeCancelled
		}
	} else {
		out = outcomeDone
	}
	if res == nil {
		return
	}
	a.log("検証 %d件: 一致 %d、不一致 %d、欠損 %d、読み取り不可 %d、所要 %s",
		res.Checked, res.Matched, len(res.Mismatch), len(res.Missing), len(res.Unreadable),
		res.Elapsed.Round(100*time.Millisecond))
	for i, f := range res.Mismatch {
		if i == 10 {
			a.log("  ... 他 %d件", len(res.Mismatch)-10)
			break
		}
		a.log("  不一致: %s", f)
	}
	for i, f := range res.Unreadable {
		if i == 10 {
			a.log("  ... 他 %d件", len(res.Unreadable)-10)
			break
		}
		a.log("  読み取り不可: %s", f)
	}
	// A cancelled verify has checked only part of the backup, so it must not
	// be reported as a clean bill of health. Neither may a file the device
	// refused to hash: nothing was compared for it, and silence about it would
	// read as "all good" - a run that hashed nothing at all used to look
	// exactly like a successful one.
	if err != nil || len(res.Mismatch) > 0 || len(res.Missing) > 0 || res.Checked == 0 {
		return
	}
	// The teardown goes first: a modal runs its own message loop, so anything
	// queued behind it waits for a click.
	finish(out)
	if n := len(res.Unreadable); n > 0 {
		a.msgBox("検証できませんでした",
			fmt.Sprintf("%d件はデバイス側でハッシュを計算できず、照合できませんでした。\n\n"+
				"一致を確認できたのは %d件です。バックアップが完全であることは確認できていません。",
				n, res.Matched),
			walk.MsgBoxIconWarning)
		return
	}
	a.msgBox("検証完了",
		fmt.Sprintf("%d件すべて一致しました。", res.Matched), walk.MsgBoxIconInformation)
}

// dropIfDead releases a connection that has already been torn down, so the next
// operation does not start on a handle that can never carry anything again.
//
// The connection is asked directly instead of the error being pattern matched.
// The substring test this replaces looked for a lowercase "usb", which the two
// errors that matter most do not contain: Abort reports "the USB connection was
// aborted" and a cancelled run reports context.Canceled while the abort that
// unblocked it has already killed the link. Either one left the window claiming
// to be connected, with every button enabled, until the user thought to press
// 再接続 - which is exactly the state the stall handling exists to avoid.
func (a *app) dropIfDead(c *adbproto.Conn) {
	if c == nil || c.Alive() {
		return
	}
	a.mu.Lock()
	mine := a.conn == c
	if mine {
		a.conn = nil
	}
	a.mu.Unlock()
	if !mine {
		// Something else already replaced it; closing it is that owner's job.
		return
	}
	c.Close()
	a.log("USB接続が失われました。「再接続」を押してください。")
	a.ui(func() {
		a.deviceLabel.SetText("未接続")
		a.refreshControls()
	})
}

func deviceName(banner string) string {
	for _, f := range strings.Split(strings.TrimPrefix(banner, "device::"), ";") {
		if v, ok := strings.CutPrefix(f, "ro.product.model="); ok {
			return v
		}
	}
	return "Android device"
}

// ellipsize shortens a path to at most max runes, from the left, so the
// filename stays visible.
//
// It counts and slices runes, not bytes. Cutting a UTF-8 string at a byte
// offset lands in the middle of a character, and since every path this label
// shows may be Japanese, the old byte slicing turned the current-file line into
// mojibake as a matter of routine.
func ellipsize(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[len(r)-max:])
	}
	// When the basename alone leaves no room for a useful prefix, show its tail
	// instead of the tail of the whole path.
	if base := []rune(filepath.Base(s)); len(base)+4 >= max {
		if len(base) > max-3 {
			base = base[len(base)-(max-3):]
		}
		return "..." + string(base)
	}
	return "..." + string(r[len(r)-(max-3):])
}
