//go:build (darwin && !ios) || (linux && !android)

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const dreamTransferOwnerTestKey = "test-only-worker-secret"

func dreamTransferOwnerTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "owner")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dreamTransferOwnerTestWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func dreamTransferOwnerTestRun(args []string, keyPath, orchestrator string) (int, string, string) {
	var out, errOutput bytes.Buffer
	code := runDreamTransferAuthority(args, &out, &errOutput, func(name string) string {
		switch name {
		case dreamTransferWorkerKeyFileEnv:
			return keyPath
		case "CONTEXTLATTICE_ORCHESTRATOR_API_KEY":
			return orchestrator
		default:
			return ""
		}
	})
	return code, out.String(), errOutput.String()
}

func TestDreamTransferAuthorityOwnerCreatesBoundPair(t *testing.T) {
	dir := dreamTransferOwnerTestDir(t)
	args := dreamTransferOwnerTestArgs(dir)
	keyPath := filepath.Join(dir, "worker-key")
	dreamTransferOwnerTestWrite(t, keyPath, dreamTransferOwnerTestKey+"\n", 0o600)
	before := time.Now().UTC()
	code, out, errOutput := dreamTransferOwnerTestRun(args, keyPath, "different-test-key")
	if code != 0 || errOutput != "" {
		t.Fatalf("provisioning failed: %s", errOutput)
	}
	fields, _ := parseDreamTransferAuthority(args)
	grantBytes, err := os.ReadFile(fields.grantFile)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := os.ReadFile(fields.proofFile)
	if err != nil {
		t.Fatal(err)
	}
	decodedProof, err := hex.DecodeString(string(proof))
	if err != nil || len(decodedProof) != 32 || string(proof) != strings.ToLower(string(proof)) {
		t.Fatal("proof must contain exactly 32 random bytes serialized as lowercase hex")
	}
	var grant dreamTransferLocalAuthority
	var output map[string]any
	if err := json.Unmarshal(grantBytes, &grant); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &output); err != nil {
		t.Fatal(err)
	}
	digest := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	wantPrincipal := "api-key-sha256:" + digest([]byte(dreamTransferOwnerTestKey)) + "/cap-sha256:" + digest(proof)
	if grant.SchemaID != "dream_transfer_local_authority.v1" || grant.Authority != "single_custodian_source_and_destination" ||
		grant.SourceProject != fields.source || grant.DestinationProject != fields.destination ||
		grant.SourcePassportID != fields.passport || grant.ExperienceRef != fields.experience || grant.SourceRecordHash != fields.digest ||
		grant.CallerPrincipal != wantPrincipal || !regexp.MustCompile("^dgrant_[a-f0-9]{32}$").MatchString(grant.GrantID) ||
		grant.IssuedAt.Before(before) || grant.IssuedAt.After(time.Now()) || grant.ExpiresAt.Sub(grant.IssuedAt) != 15*time.Minute {
		t.Fatal("grant does not bind the exact input and two independent secrets")
	}
	if len(output) != 2 || output["grant_id"] != grant.GrantID || output["expires_at"] != grant.ExpiresAt.Format(time.RFC3339Nano) ||
		strings.Contains(out, string(proof)) || bytes.Contains(grantBytes, proof) || bytes.Contains(grantBytes, []byte(dreamTransferOwnerTestKey)) {
		t.Fatal("receipt or grant exposed unexpected information")
	}
	for _, path := range []string{fields.grantFile, fields.proofFile} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode() != 0o600 {
			t.Fatal("outputs are not regular 0600 files")
		}
	}
	code, out, _ = dreamTransferOwnerTestRun(args, keyPath, "")
	if code == 0 || out != "" {
		t.Fatal("existing pair was overwritten")
	}
	if after, _ := os.ReadFile(fields.grantFile); !bytes.Equal(after, grantBytes) {
		t.Fatal("retry changed existing grant")
	}
	if after, _ := os.ReadFile(fields.proofFile); !bytes.Equal(after, proof) {
		t.Fatal("retry changed existing proof")
	}

	args[11], args[13] = filepath.Join(dir, "another-grant.json"), filepath.Join(dir, "another-proof")
	code, out, _ = dreamTransferOwnerTestRun(args, keyPath, "")
	if code != 0 || strings.Contains(out, grant.GrantID) {
		t.Fatal("new pair did not receive a unique grant ID")
	}
	if after, _ := os.ReadFile(args[13]); bytes.Equal(after, proof) {
		t.Fatal("new pair reused an old capability")
	}
}

func TestDreamTransferAuthorityOwnerRefusesUnsafePathsAndKey(t *testing.T) {
	for _, scenario := range []string{
		"missing-key", "same-orchestrator-key", "insecure-key-mode", "oversized-key", "multiline-key",
		"key-symlink", "key-hardlink", "key-fifo", "public-directory", "symlink-directory", "missing-directory",
		"existing-grant", "symlink-grant",
	} {
		t.Run(scenario, func(t *testing.T) {
			dir := dreamTransferOwnerTestDir(t)
			args := dreamTransferOwnerTestArgs(dir)
			keyPath := filepath.Join(dir, "worker-key")
			dreamTransferOwnerTestWrite(t, keyPath, dreamTransferOwnerTestKey, 0o600)
			var orchestrator string
			switch scenario {
			case "missing-key":
				keyPath = ""
			case "same-orchestrator-key":
				orchestrator = dreamTransferOwnerTestKey
			case "insecure-key-mode":
				dreamTransferOwnerTestWrite(t, keyPath, dreamTransferOwnerTestKey, 0o640)
			case "oversized-key":
				dreamTransferOwnerTestWrite(t, keyPath, strings.Repeat("x", 4097), 0o600)
			case "multiline-key":
				dreamTransferOwnerTestWrite(t, keyPath, dreamTransferOwnerTestKey+"\nsecond-line", 0o600)
			case "key-symlink", "key-hardlink":
				link := filepath.Join(dir, "key-link")
				makeLink := os.Symlink
				if scenario == "key-hardlink" {
					makeLink = os.Link
				}
				if err := makeLink(keyPath, link); err != nil {
					t.Fatal(err)
				}
				keyPath = link
			case "key-fifo":
				keyPath = filepath.Join(dir, "key-fifo")
				if err := unix.Mkfifo(keyPath, 0o600); err != nil {
					t.Fatal(err)
				}
			case "public-directory", "symlink-directory", "missing-directory":
				outputDir := filepath.Join(dir, "out")
				if scenario == "public-directory" {
					if err := os.Mkdir(outputDir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(outputDir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "symlink-directory" {
					if err := os.Symlink(dir, outputDir); err != nil {
						t.Fatal(err)
					}
				}
				args[11] = filepath.Join(outputDir, "grant.json")
			case "existing-grant":
				dreamTransferOwnerTestWrite(t, args[11], "do not overwrite", 0o600)
			case "symlink-grant":
				if err := os.Symlink(keyPath, args[11]); err != nil {
					t.Fatal(err)
				}
			}
			code, out, errOutput := dreamTransferOwnerTestRun(args, keyPath, orchestrator)
			if code == 0 || out != "" || errOutput == "" || strings.Contains(errOutput, dreamTransferOwnerTestKey) {
				t.Fatal("unsafe input was accepted or leaked")
			}
			if _, err := os.Lstat(args[13]); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected invocation left a proof")
			}
			if scenario == "existing-grant" {
				if current, _ := os.ReadFile(args[11]); string(current) != "do not overwrite" {
					t.Fatal("rejection changed an existing grant")
				}
			}
		})
	}
}

func TestDreamTransferAuthorityOwnerRollsBackOnLateGrantCollision(t *testing.T) {
	dir := dreamTransferOwnerTestDir(t)
	grantPath, proofPath := filepath.Join(dir, "grant.json"), filepath.Join(dir, "proof")
	func() {
		proof, err := openDreamTransferAuthorityFile(proofPath)
		if err != nil {
			t.Fatal(err)
		}
		defer proof.close()
		grant, err := openDreamTransferAuthorityFile(grantPath)
		if err != nil {
			t.Fatal(err)
		}
		defer grant.close()
		if err := proof.stage([]byte("test-capability")); err != nil {
			t.Fatal(err)
		}
		if err := grant.stage([]byte("test-grant")); err != nil {
			t.Fatal(err)
		}
		dreamTransferOwnerTestWrite(t, grantPath, "concurrent owner's grant", 0o600)
		if err := publishDreamTransferAuthorityPair(proof, grant, time.Now().Add(time.Minute)); err == nil || !proof.published {
			t.Fatal("control did not fail after publishing proof and before publishing grant")
		}
		if info, err := proof.handle.Stat(); err != nil || info.Size() != 0 {
			t.Fatal("rollback did not invalidate its own proof")
		}
	}()
	if current, err := os.ReadFile(grantPath); err != nil || string(current) != "concurrent owner's grant" {
		t.Fatal("rollback removed or changed a grant it did not own")
	}
	if _, err := os.Lstat(proofPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback left partial proof output")
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 1 {
		t.Fatal("rollback left temporary outputs")
	}
}

func TestDreamTransferAuthorityOwnerExpiryPreventsGrantPublication(t *testing.T) {
	dir := dreamTransferOwnerTestDir(t)
	grant, proof := filepath.Join(dir, "grant.json"), filepath.Join(dir, "proof")
	if err := writeDreamTransferAuthorityPair(grant, proof, []byte("grant"), []byte("capability"), time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expired authority was published")
	}
	for _, path := range []string{grant, proof} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("expired authority left partial output")
		}
	}
}

func TestDreamTransferAuthorityOwnerConcurrentPairDoesNotOverwrite(t *testing.T) {
	dir := dreamTransferOwnerTestDir(t)
	args := dreamTransferOwnerTestArgs(dir)
	key := filepath.Join(dir, "worker-key")
	dreamTransferOwnerTestWrite(t, key, dreamTransferOwnerTestKey, 0o600)
	const callers = 6
	start, receipts := make(chan struct{}), make(chan int, callers)
	for range callers {
		go func() {
			<-start
			code, _, _ := dreamTransferOwnerTestRun(args, key, "")
			receipts <- code
		}()
	}
	close(start)
	success := 0
	for range callers {
		if <-receipts == 0 {
			success++
		}
	}
	names, err := os.ReadDir(dir)
	if err != nil || success != 1 || len(names) != 3 {
		t.Fatalf("concurrent issuance should leave one pair and its key: successes=%d, entries=%d, error=%v", success, len(names), err)
	}
}
