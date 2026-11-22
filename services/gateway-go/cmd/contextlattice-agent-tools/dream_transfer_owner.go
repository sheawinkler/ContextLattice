package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	dreamTransferWorkerKeyFileEnv = "CONTEXTLATTICE_DREAM_WORKER_KEY_FILE"
	dreamTransferMaxAuthorityTTL  = 15 * time.Minute
)

var dreamTransferSourceDigest = regexp.MustCompile("^sha256:[0-9a-f]{64}$")

type dreamTransferLocalAuthority struct {
	SchemaID           string    `json:"schema_id"`
	GrantID            string    `json:"grant_id"`
	SourceProject      string    `json:"source_project"`
	DestinationProject string    `json:"destination_project"`
	SourcePassportID   string    `json:"source_passport_id"`
	ExperienceRef      string    `json:"experience_ref"`
	SourceRecordHash   string    `json:"source_record_hash"`
	CallerPrincipal    string    `json:"caller_principal"`
	IssuedAt           time.Time `json:"issued_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	Authority          string    `json:"authority"`
}

// runCLIDreamTransferAuthority receives only the arguments after "dream authorize-local".
func runCLIDreamTransferAuthority(args []string) int {
	return runDreamTransferAuthority(args, os.Stdout, os.Stderr, os.Getenv)
}

func runDreamTransferAuthority(args []string, stdout, stderr io.Writer, lookupEnv func(string) string) int {
	if !dreamTransferAuthoritySupported {
		fmt.Fprintln(stderr, "dream authorize-local is available only on Darwin and Linux")
		return 1
	}
	fields, err := parseDreamTransferAuthority(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	keyPath := lookupEnv(dreamTransferWorkerKeyFileEnv)
	key, err := readDreamTransferWorkerKey(keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "dream authorize-local requires a private worker-key file via "+dreamTransferWorkerKeyFileEnv)
		return 1
	}
	for _, name := range []string{"CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "MEMMCP_ORCHESTRATOR_API_KEY"} {
		if key == strings.TrimSpace(lookupEnv(name)) {
			fmt.Fprintln(stderr, "dream authorize-local requires a separate worker key")
			return 1
		}
	}

	var grantRandom [16]byte
	var proofRandom [32]byte
	if _, err := rand.Read(grantRandom[:]); err != nil {
		fmt.Fprintln(stderr, "dream authorize-local could not generate authority")
		return 1
	}
	if _, err := rand.Read(proofRandom[:]); err != nil {
		fmt.Fprintln(stderr, "dream authorize-local could not generate authority")
		return 1
	}
	proof := hex.EncodeToString(proofRandom[:])
	issued := time.Now().UTC()
	expires := issued.Add(fields.ttl)
	grant := dreamTransferLocalAuthority{
		SchemaID: "dream_transfer_local_authority.v1", GrantID: "dgrant_" + hex.EncodeToString(grantRandom[:]),
		SourceProject: fields.source, DestinationProject: fields.destination, SourcePassportID: fields.passport,
		ExperienceRef: fields.experience, SourceRecordHash: fields.digest,
		CallerPrincipal: "api-key-sha256:" + dreamTransferAuthorityDigest(key) + "/cap-sha256:" + dreamTransferAuthorityDigest(proof),
		IssuedAt:        issued, ExpiresAt: expires, Authority: "single_custodian_source_and_destination",
	}
	contents, err := json.Marshal(grant)
	if err != nil || writeDreamTransferAuthorityPair(fields.grantFile, fields.proofFile, append(contents, '\n'), []byte(proof), expires) != nil {
		fmt.Fprintln(stderr, "dream authorize-local could not publish the grant and proof")
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		GrantID   string    `json:"grant_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}{grant.GrantID, expires}); err != nil {
		return 1
	}
	return 0
}

func dreamTransferAuthorityDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type dreamTransferAuthorityArgs struct {
	source, destination, passport, experience, digest, grantFile, proofFile string
	ttl                                                                     time.Duration
}

func parseDreamTransferAuthority(args []string) (dreamTransferAuthorityArgs, error) {
	var out dreamTransferAuthorityArgs
	var ttl string
	flags := map[string]*string{
		"--source": &out.source, "--destination": &out.destination, "--passport": &out.passport,
		"--experience": &out.experience, "--source-digest": &out.digest, "--grant-file": &out.grantFile,
		"--proof-file": &out.proofFile, "--ttl": &ttl,
	}
	invalid := errors.New("dream authorize-local requires --source, --destination, --passport, --experience, --source-digest, --grant-file, --proof-file and --ttl")
	if len(args) > 2*len(flags) {
		return out, invalid
	}
	for len(args) != 0 {
		name, value, joined := strings.Cut(args[0], "=")
		args = args[1:]
		target, ok := flags[name]
		if !ok {
			return out, invalid
		}
		if !joined {
			if len(args) == 0 || strings.HasPrefix(args[0], "--") {
				return out, invalid
			}
			value, args = args[0], args[1:]
		}
		*target = value
		delete(flags, name)
	}
	if len(flags) != 0 {
		return out, invalid
	}
	if !validDreamTransferProject(out.source) || !validDreamTransferProject(out.destination) || strings.EqualFold(out.source, out.destination) {
		return out, errors.New("dream authorize-local requires two distinct, exact project names")
	}
	if !validDreamTransferAuthorityText(out.passport, 128) || !validDreamTransferAuthorityText(out.experience, 256) ||
		!dreamTransferSourceDigest.MatchString(out.digest) {
		return out, errors.New("dream authorize-local requires bounded references and a lowercase sha256 source digest")
	}
	if !validDreamTransferAuthorityPath(out.grantFile) || !validDreamTransferAuthorityPath(out.proofFile) || out.grantFile == out.proofFile {
		return out, errors.New("dream authorize-local requires distinct, absolute grant and proof file paths")
	}
	var err error
	out.ttl, err = time.ParseDuration(ttl)
	if err != nil || out.ttl <= 0 || out.ttl > dreamTransferMaxAuthorityTTL {
		return out, errors.New("dream authorize-local requires a positive --ttl no longer than 15m")
	}
	return out, nil
}

func validDreamTransferProject(value string) bool {
	return validDreamTransferAuthorityText(value, 120) && !strings.ContainsAny(value, "/\\") &&
		!strings.Contains(value, "::") && !strings.HasPrefix(value, "_") && value != "." && value != ".."
}

func validDreamTransferAuthorityText(value string, limit int) bool {
	return len(value) > 0 && len(value) <= limit && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}

func validDreamTransferAuthorityPath(path string) bool {
	return validDreamTransferAuthorityText(path, 4096) && filepath.IsAbs(path) &&
		filepath.Clean(path) == path && filepath.Base(path) != string(filepath.Separator)
}
