# QuickADBackup

Android端末の共有ストレージをPCへ**差分**バックアップするWindows用ツール。ADB
プロトコルをUSB上で直接話すので、**adb.exeもadbサーバーもcgoも要らない**。

`adb pull -a /sdcard` は毎回すべてを転送し直す。比較する相手を持っていないので
それ以外にやりようがない。このツールは先に端末側のメタデータを読んで何が変わった
かを確かめ、変わった分だけ取ってくる。

**端末は読み取り専用。** 端末上には何も作らず、書き換えず、消さない。端末から消え
たファイルはPC側の日付付きフォルダへ退避するだけで、どこでも削除しない。

## 速度

Pixel 7 / Android 16 / USB 3、`Pictures` 7,204ファイル・1.1GiB:

| | adb経由 | このツール |
| --- | --- | --- |
| 初回バックアップ | 49.3秒 | **18.0秒** |
| 2回目（変更なし） | 1.8秒 | 1.8秒 |
| 転送速度 | 15.3MiB/s | **101.7MiB/s** |

並列ストリーム数（`-workers`）を増やすと 1本で117ファイル/秒、8本で817ファイル/秒。
既定は8本。

## 使い方

**`quickadbackup-gui.exe`** — 保存先を選んで実行するだけのウィンドウ。進捗と転送
速度が出て、途中で止められる。保存先は次回も覚えていて、ログは
`%APPDATA%\QuickADBackup\gui.log` にも残る。

**`quickadbackup.exe`** — スクリプトや定期実行向けのCLI。

```
quickadbackup probe                    # 端末が何に対応しているか見る
quickadbackup sync -dest D:\Backup     # 新規・変更分をコピー
quickadbackup verify -dest D:\Backup   # バックアップを端末と照合し直す
```

| フラグ | 意味 |
| --- | --- |
| `-dest DIR` | PC側の保存先（必須） |
| `-root DIR` | 端末側の対象フォルダ（既定 `/storage/emulated/0`） |
| `-exclude LIST` | root からの相対パスをカンマ区切りで除外 |
| `-workers N` | 並列転送ストリーム数（既定 8） |
| `-dry-run` | 何をするかだけ表示して転送しない |
| `-sample N` | `verify` 用。無作為に N 件だけ照合（`0` で全件） |
| `-device FRAG` | 複数台つながっているときの選択 |

`Android/data` と `Android/obb` は既定で除外する。adbのシェルユーザーからは読めず、
中身はアプリのキャッシュなので（テスト端末で12,960ファイル）。

Git Bash や MSYS では先に `MSYS_NO_PATHCONV=1` を設定すること。しないとシェルが
`/storage/...` をWindowsパスに書き換えてしまう。

中断した実行は途中から再開する。Ctrl+C は1回目が中止（集計を表示して終わる）、
2回目が即時終了。

`sync` と `verify` は、確かめようとしたことを確かめられなかった実行を失敗として
扱い、非ゼロで終了する。読めなかったファイルが1つでもあれば失敗。定期実行が見るの
は終了ステータスだけなので、そこが「正常」と言ってはならない。

### 制約: adbサーバーと同時には使えない

USBインターフェースは1プロセスしか掴めない。**adbサーバーを止めてから**使うこと。
Android Studio や scrcpy が端末を掴んでいる間も同じ。

またadbdはホストが切断するとUSB機能を落として上げ直すので、インターフェースが
Windowsから3.6秒ほど消える。続けて実行した場合はその分待つ（待っている旨を出す）。

## 仕組み

| 層 | パッケージ |
| --- | --- |
| USB | `internal/winusb` — WinUSBドライバ経由でADBインターフェースを掴み、バルク転送する |
| プロトコル | `internal/adbproto` — CNXN/AUTH/OPEN/OKAY/WRTE/CLSE、RSA認証、ストリーム多重化 |
| ファイル転送 | `internal/adbproto/sync.go` — `STAT_V2` / `LIST_V2` / `RECV` |

リバースエンジニアリングではない。AOSPの `packages/modules/adb` にある
`protocol.txt`・`SERVICES.TXT`・`SYNC.TXT` の通りに実装している。

認証はadbと同じ `~/.android/adbkey` を使うので、既にこのPCを許可済みの端末なら
許可し直す必要はない。鍵が無ければadbと同じ形式（PKCS#8 PEM と 524バイトの
`RSAPublicKey`）で生成するため、どちらのツールからでも使える。

ファイルの列挙は sync の `LIST` ではなく `find` 一発で済ませている。`LIST` は
ディレクトリごとに往復するので、7,959ディレクトリで29.1秒かかった（`find` なら
2.9秒）。

実機で踏んだ落とし穴 — `find /sdcard` が何も返さない、`shell` サービスがバイナリ
を壊す、`tar` が読めないファイルを黙って飛ばす、Windowsで使えない文字を含む
ファイル名、など — への対処は、いずれも理由込みでコード中のコメントに書いてある。

## ビルド

```
go build -o quickadbackup.exe .
go build -ldflags "-H=windowsgui" -o quickadbackup-gui.exe ./cmd/gui
```

`cmd/gui/rsrc.syso` は `app.manifest`（Common Controls 6.0）を埋め込んだもので、
これが無いとGUIはウィンドウすら開かない。マニフェストを変えたら作り直すこと:

```
go run github.com/akavel/rsrc@latest -manifest cmd/gui/app.manifest -arch amd64 -o cmd/gui/rsrc.syso
```

## ライセンス

MIT。[LICENSE](LICENSE) を参照。
