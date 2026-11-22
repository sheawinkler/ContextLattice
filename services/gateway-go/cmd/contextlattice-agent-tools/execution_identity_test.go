package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExecutionIndexedThread(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("host sqlite3 unavailable")
	}
	root := filepath.Join(t.TempDir(), "profile[1]")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SQLITE_HOME", root)
	for _, k := range []string{"CONTEXTLATTICE_HARNESS", "CONTEXTLATTICE_REASONING_EFFORT", "CONTEXTLATTICE_NATIVE_SESSION_ID", "CONTEXTLATTICE_MODEL_FAMILY", "CONTEXTLATTICE_MODEL_ORIGIN", "CONTEXTLATTICE_MODEL_VERSION"} {
		t.Setenv(k, "")
	}
	const id = "00000000-0000-0000-0000-000000000001"
	const other = "00000000-0000-0000-0000-000000000002"
	db := filepath.Join(root, "state_5.sqlite")
	q := func(sql string) {
		t.Helper()
		if b, err := exec.Command(sqlite, "-init", os.DevNull, db, sql).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	q("CREATE TABLE threads(id TEXT PRIMARY KEY,model TEXT,reasoning_effort TEXT,archived INTEGER); INSERT INTO threads VALUES ('" + id + "','lab-2.1-birch','max',0),('" + other + "','lab-3.0-ash','low',0)")
	t.Setenv("CODEX_THREAD_ID", id)
	check := func(agent, model, native, want string) {
		t.Helper()
		p := parsedArgs{values: map[string]string{"native_session_id": native}}
		f := invocationFacts(p, agent, model).Metadata("stable-owner")
		got, _ := f["execution_label"].(string)
		if got != want {
			t.Fatalf("wrong attribution: %#v", f)
		}
	}
	check("agent-cli", "", "", "codex-lab-birch-2.1-max")
	check("codex", "lab-2.1-birch", id, "codex-lab-birch-2.1-max")
	check("codex", "lab-2.1-ash", id, "")
	check("codex", "", other, "")
	check("hermes-agent", "lab-2.1-birch", "", "")
	t.Run("inherited configuration cannot outrank the active thread or cross a harness", func(t *testing.T) {
		t.Setenv("CONTEXTLATTICE_HARNESS", "codex")
		t.Setenv("CONTEXTLATTICE_MODEL", "lab-1.0-old")
		t.Setenv("CONTEXTLATTICE_REASONING_EFFORT", "low")
		check("codex", "lab-1.0-old", id, "codex-lab-birch-2.1-max")
		p := parsedArgs{values: map[string]string{"agent": "hermes-agent", "model": "lab-3.0-ash"}}
		m := invocationFacts(p, "hermes-agent", "lab-3.0-ash").Metadata("owner")
		if m["harness"] != "hermes" || m["reasoning_effort"] != nil || m["execution_label"] != nil {
			t.Fatalf("inherited a foreign runtime: %#v", m)
		}
		p.values["reasoning_effort"] = "medium"
		if label := invocationFacts(p, "hermes-agent", "lab-3.0-ash").Metadata("owner")["execution_label"]; label != "hermes-lab-ash-3.0-medium" {
			t.Fatalf("explicit invocation lost: %v", label)
		}
	})
	q("UPDATE threads SET model='lab-3.0-ash',reasoning_effort='low' WHERE id='" + id + "'")
	check("codex", "", "", "codex-lab-ash-3.0-low")
	q("UPDATE threads SET archived=1 WHERE id='" + id + "'")
	check("codex", "", "", "")
	t.Setenv("CODEX_THREAD_ID", other)
	if err := os.WriteFile(filepath.Join(root, "state_6.sqlite"), []byte("unreadable schema"), 0600); err != nil {
		t.Fatal(err)
	}
	check("codex", "", "", "")
	t.Setenv("PATH", t.TempDir())
	check("codex", "", "", "")
	t.Setenv("CODEX_THREAD_ID", "not a thread")
	check("codex", "", "", "")
}
