# QuickADBackup

Android端末の共有ストレージをPCへ差分バックアップするWindows用ツールです。

## 使い方

`quickadbackup-gui.exe` を起動すると、保存先を選んで実行するウィンドウが開きます。

`quickadbackup.exe` はコマンドラインから実行します。スクリプトや定期実行向けです。

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

`Android/data` と `Android/obb` は既定で除外します。

### 使用前の準備

USBインターフェースは1つのプロセスが専有します。実行前にadbサーバーを停止し、Android Studio や scrcpy からの接続も解除してください。

## 速度

Pixel 7 / Android 16 / USB 3、`Pictures` 7,204ファイル・1.1GiB:

| | adb経由 | このツール |
| --- | --- | --- |
| 初回バックアップ | 49.3秒 | 18.0秒 |
| 2回目（変更なし） | 1.8秒 | 1.8秒 |
| 転送速度 | 15.3MiB/s | 101.7MiB/s |

## ビルド

```
go build -o quickadbackup.exe .
go build -ldflags "-H=windowsgui" -o quickadbackup-gui.exe ./cmd/gui
```

`cmd/gui/rsrc.syso` は `app.manifest` を埋め込んだもので、GUIの起動に必要です。

## ライセンス

MIT
