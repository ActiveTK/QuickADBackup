# QuickADBackup

Android端末の共有ストレージをPCへ差分バックアップするWindows用ツールです。ADB
プロトコルをUSB上で直接話すため、必要なものはWinUSBドライバだけです。

`adb pull -a /sdcard` は実行のたびに全ファイルを転送します。このツールは先に端末
側のメタデータを読んで変更点を確かめ、変わった分だけを取得します。

端末に対しては読み取りのみを行います。端末から消えたファイルは、PC側の日付付き
フォルダへ退避します。

## 速度

Pixel 7 / Android 16 / USB 3、`Pictures` 7,204ファイル・1.1GiB:

| | adb経由 | このツール |
| --- | --- | --- |
| 初回バックアップ | 49.3秒 | 18.0秒 |
| 2回目（変更なし） | 1.8秒 | 1.8秒 |
| 転送速度 | 15.3MiB/s | 101.7MiB/s |

並列ストリーム数（`-workers`）は1本で117ファイル/秒、8本で817ファイル/秒になり
ます。既定は8本です。

## 使い方

`quickadbackup-gui.exe` は、保存先を選んで実行するウィンドウです。進捗と転送速度
を表示し、途中で停止できます。保存先は次回も引き継ぎ、ログは
`%APPDATA%\QuickADBackup\gui.log` にも残ります。

`quickadbackup.exe` は、スクリプトや定期実行向けのCLIです。

```
quickadbackup probe                    # 端末の対応状況を確認
quickadbackup sync -dest D:\Backup     # 新規・変更分をコピー
quickadbackup verify -dest D:\Backup   # バックアップを端末と照合
```

| フラグ | 意味 |
| --- | --- |
| `-dest DIR` | PC側の保存先（必須） |
| `-root DIR` | 端末側の対象フォルダ（既定 `/storage/emulated/0`） |
| `-exclude LIST` | root からの相対パスをカンマ区切りで除外 |
| `-workers N` | 並列転送ストリーム数（既定 8） |
| `-dry-run` | 実行内容の表示のみ |
| `-sample N` | `verify` 用。無作為に N 件を照合（`0` で全件） |
| `-device FRAG` | 複数台接続時の選択 |

`Android/data` と `Android/obb` は既定で除外します。adbのシェルユーザーからは
読み取りが制限されており、中身はアプリのキャッシュです（テスト端末で12,960
ファイル）。

Git Bash や MSYS では、先に `MSYS_NO_PATHCONV=1` を設定してください。シェルが
`/storage/...` をWindowsパスへ書き換えるのを防げます。

中断した実行は途中から再開します。Ctrl+C は1回目が中止（集計を表示して終了しま
す）、2回目が即時終了です。

`sync` と `verify` は、確かめようとしたことを確かめられた場合にのみ 0 で終了しま
す。読み取りに失敗したファイルが1件でもあれば、失敗として非ゼロで終了します。
定期実行が参照するのは終了ステータスだけなので、実際に照合できた範囲だけを正常と
して報告します。

### 使用前の準備

USBインターフェースは1つのプロセスが専有します。実行前にadbサーバーを停止し、
Android Studio や scrcpy からの接続も解除してください。

adbdはホストの切断に応じてUSB機能を再起動するため、インターフェースがWindowsから
3.6秒ほど姿を消します。続けて実行した場合はその間待機し、待機中はその旨を表示しま
す。

## ビルド

```
go build -o quickadbackup.exe .
go build -ldflags "-H=windowsgui" -o quickadbackup-gui.exe ./cmd/gui
```

`cmd/gui/rsrc.syso` は `app.manifest`（Common Controls 6.0）を埋め込んだもので、
GUIの起動に必要です。マニフェストを変更した場合は作り直してください:

```
go run github.com/akavel/rsrc@latest -manifest cmd/gui/app.manifest -arch amd64 -o cmd/gui/rsrc.syso
```

## ライセンス

MIT
