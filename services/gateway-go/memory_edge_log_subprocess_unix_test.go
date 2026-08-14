//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const memoryEdgeLogSubprocessHelperEnv = "CONTEXTLATTICE_TEST_EDGE_LOG_SUBPROCESS_HELPER"

func TestMemoryEdgeLogSubprocessAppendHelper(t *testing.T) {
	if os.Getenv(memoryEdgeLogSubprocessHelperEnv) == "replace" {
		lockPath := os.Getenv("CONTEXTLATTICE_TEST_EDGE_LOG_LOCK_PATH")
		unlock, err := lockOwnerOnlyFileContext(context.Background(), lockPath)
		if err != nil {
			t.Fatalf("replacement helper lock acquisition: %v", err)
		}
		fmt.Println("edge-log-replacement-lock-acquired")
		unlock()
		return
	}
	if os.Getenv(memoryEdgeLogSubprocessHelperEnv) != "1" {
		t.Skip("subprocess helper")
	}
	edgePath := os.Getenv("CONTEXTLATTICE_TEST_EDGE_LOG_PATH")
	var edge memoryEdgeEntry
	if edgePath == "" || json.Unmarshal([]byte(os.Getenv("CONTEXTLATTICE_TEST_EDGE_LOG_ENTRY")), &edge) != nil {
		t.Fatal("subprocess edge-log fixture is invalid")
	}
	store := &memoryStore{policy: memoryStorePolicy{edgePath: edgePath}}
	probe, err := os.OpenFile(memoryEdgeLogFencePath(store), os.O_CREATE|os.O_RDWR, ownerOnlyFileMode)
	if err != nil {
		t.Fatalf("open subprocess flock probe: %v", err)
	}
	probeErr := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if probeErr == nil {
		_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)
		_ = probe.Close()
		t.Fatal("subprocess acquired writer fence while parent compaction held it")
	}
	_ = probe.Close()
	if !errors.Is(probeErr, unix.EWOULDBLOCK) && !errors.Is(probeErr, unix.EAGAIN) {
		t.Fatalf("subprocess flock probe returned unexpected error: %v", probeErr)
	}
	fmt.Println("edge-log-flock-contended")
	if _, _, err := store.appendMemoryEdgeLog(edge, true); err != nil {
		t.Fatalf("subprocess append after compaction fence: %v", err)
	}
}

func TestMemoryEdgeLogFencePathReplacementFailsClosedAcrossOSProcess(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	if _, _, err := store.appendMemoryEdgeLog(testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md"), true); err != nil {
		t.Fatalf("seed replacement-process edge log: %v", err)
	}
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("acquire replacement-process fence: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			fence.release()
		}
	}()
	lockPath := memoryEdgeLogFencePath(store)
	replacedPath := lockPath + ".process-replaced"
	if err := os.Rename(lockPath, replacedPath); err != nil {
		t.Fatalf("replace lock pathname across process: %v", err)
	}
	defer func() {
		_ = os.Remove(lockPath)
		_ = os.Rename(replacedPath, lockPath)
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve replacement helper: %v", err)
	}
	command := exec.Command(executable, "-test.run=^TestMemoryEdgeLogSubprocessAppendHelper$", "-test.count=1")
	command.Env = append(os.Environ(),
		memoryEdgeLogSubprocessHelperEnv+"=replace",
		"CONTEXTLATTICE_TEST_EDGE_LOG_LOCK_PATH="+lockPath,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open replacement helper stdout: %v", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start replacement helper: %v", err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	if readErr != nil || strings.TrimSpace(line) != "edge-log-replacement-lock-acquired" {
		_ = command.Wait()
		t.Fatalf("replacement helper did not acquire replacement pathname: line=%q read_err=%v stderr=%s", line, readErr, stderr.String())
	}
	if _, err := store.newMemoryEdgeLogAppenderFastWithFenceLocked(true, fence); !errors.Is(err, errOwnerOnlyLockPathChanged) {
		_ = command.Wait()
		t.Fatalf("parent stale descriptor did not fail closed after pathname replacement: %v", err)
	}
	fence.release()
	locked = false
	if err := command.Wait(); err != nil {
		t.Fatalf("replacement helper failed: %v stderr=%s", err, stderr.String())
	}
}

func TestMemoryEdgeLogFenceAcrossOSProcessAppendAndCompaction(t *testing.T) {
	server, gateway := newMemoryGraphTestServer(t, true)
	defer gateway.Close()
	store := server.memoryStore
	first := testMemoryEdgeLogEntry("alpha::notes/a.md", "alpha::notes/b.md")
	if _, _, err := store.appendMemoryEdgeLog(first, true); err != nil {
		t.Fatalf("seed subprocess edge log: %v", err)
	}
	fence, err := store.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatalf("lock parent compaction fence: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			fence.release()
		}
	}()
	before, err := store.snapshotMemoryEdgeLogLocked(0)
	if err != nil {
		t.Fatalf("snapshot under parent compaction fence: %v", err)
	}
	second := testMemoryEdgeLogEntry("alpha::notes/b.md", "alpha::notes/c.md")
	edgeRaw, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal subprocess edge: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	command := exec.Command(executable, "-test.run=^TestMemoryEdgeLogSubprocessAppendHelper$", "-test.count=1")
	command.Env = append(os.Environ(),
		memoryEdgeLogSubprocessHelperEnv+"=1",
		"CONTEXTLATTICE_TEST_EDGE_LOG_PATH="+store.policy.edgePath,
		"CONTEXTLATTICE_TEST_EDGE_LOG_ENTRY="+string(edgeRaw),
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open subprocess stdout: %v", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start subprocess append: %v", err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	if readErr != nil || strings.TrimSpace(line) != "edge-log-flock-contended" {
		fence.release()
		locked = false
		waitErr := command.Wait()
		t.Fatalf("subprocess did not prove OS-level contention: line=%q read_err=%v wait_err=%v stderr=%s", line, readErr, waitErr, stderr.String())
	}
	if _, err := store.replaceMemoryEdgeLogWithFenceLocked(before.Bytes, "subprocess_compaction", fence); err != nil {
		fence.release()
		locked = false
		_ = command.Wait()
		t.Fatalf("compact while subprocess is fenced: %v", err)
	}
	fence.release()
	locked = false
	if err := command.Wait(); err != nil {
		t.Fatalf("subprocess append failed after compaction unlock: %v stderr=%s", err, stderr.String())
	}
	final, err := store.snapshotMemoryEdgeLog(0)
	if err != nil {
		t.Fatalf("snapshot after subprocess append: %v", err)
	}
	if !bytes.Contains(final.Bytes, []byte(first.SourceID)) || !bytes.Contains(final.Bytes, []byte(second.TargetID)) || final.Generation < before.Generation+2 || final.ContentDigest != memoryEdgeLogContentDigest(final.Bytes) {
		t.Fatalf("cross-process compaction/append lost exact state: before=%#v final=%#v log=%s", before, final, final.Bytes)
	}
}
