package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settings is the small amount of state worth surviving a restart, so the
// destination does not have to be typed again every session.
type settings struct {
	Dest string `json:"dest"`
	Root string `json:"root"`
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "QuickADBackup", "settings.json"), nil
}

// LogPath is where the session log is mirrored, so a completed backup leaves a
// record that outlives the window.
func LogPath() (string, error) {
	p, err := settingsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "gui.log"), nil
}

// appendLog mirrors one line to the log file. Failures are ignored: losing the
// file copy must never disturb the window.
func appendLog(line string) {
	p, err := LogPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\r\n")
}

func loadSettings() settings {
	var s settings
	p, err := settingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return s
	}
	// A corrupt file is not worth reporting: the fields simply stay empty.
	json.Unmarshal(data, &s)
	return s
}

func saveSettings(s settings) {
	p, err := settingsPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(p, data, 0o644)
}
