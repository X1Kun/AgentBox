package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/X1Kun/AgentBox/internal/session"
)

func TestReportIsAtomicPrivateAndPreservesPreviousIntentOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	first := session.CleanupReport{Version: 1, Entries: []session.CleanupEntry{{UID: "old-uid", ResourceVersion: "42", Outcome: "delete_planned"}}}
	if err := writeReport(path, first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report is not private")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved session.CleanupReport
	if json.Unmarshal(data, &saved) != nil || saved.Entries[0].Outcome != "delete_planned" {
		t.Fatal("intent not saved")
	}
	if err := writeReport(filepath.Join(dir, "missing", "report.json"), session.CleanupReport{}); err == nil {
		t.Fatal("write failure ignored")
	}
	still, err := os.ReadFile(path)
	if err != nil || string(still) != string(data) {
		t.Fatal("failed write damaged saved intent")
	}
	first.Entries[0].Outcome = "deleted"
	if err := writeReport(path, first); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, ".agentbox-cleanup-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary audit files leaked")
	}
}
