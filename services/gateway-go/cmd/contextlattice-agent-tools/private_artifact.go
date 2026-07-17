package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

var privateArtifactReplace = replacePrivateArtifact
var privateArtifactSecure = securePrivateArtifactFile

const privateArtifactTempAttempts = 128

type privateArtifactPublication struct {
	file      *os.File
	committed bool
	commit    func() error
	cleanup   func()
}

func replacePrivateArtifact(publication *privateArtifactPublication) error {
	return publication.commit()
}

func privateArtifactTempName(targetName string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "." + targetName + ".tmp-" + hex.EncodeToString(random[:]), nil
}

func validatePrivateArtifactWindowsLeaf(targetName string) error {
	if strings.TrimSpace(targetName) == "" || targetName == "." || targetName == ".." {
		return errors.New("private artifact Windows target name is invalid")
	}
	if strings.HasSuffix(targetName, ".") || strings.HasSuffix(targetName, " ") {
		return errors.New("private artifact Windows target name must not end in a dot or space")
	}
	for _, character := range targetName {
		if character < 32 || character == 127 || strings.ContainsRune(`<>:"/\|?*`, character) {
			return errors.New("private artifact Windows target name contains an invalid character")
		}
	}
	base := targetName
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	base = strings.ToUpper(strings.TrimRight(base, ". "))
	reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		base == "CLOCK$" || base == "CONIN$" || base == "CONOUT$"
	runes := []rune(base)
	if len(runes) == 4 && (string(runes[:3]) == "COM" || string(runes[:3]) == "LPT") &&
		((runes[3] >= '1' && runes[3] <= '9') || runes[3] == '\u00b9' || runes[3] == '\u00b2' || runes[3] == '\u00b3') {
		reserved = true
	}
	if reserved {
		return errors.New("private artifact Windows target name is a reserved device name")
	}
	return nil
}

func validatePrivateArtifactWindowsNamespace(path string) error {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(path), "/", `\`))
	for _, prefix := range []string{`\\.\`, `\\?\`, `\??\`, `\\??\`} {
		if strings.HasPrefix(normalized, prefix) {
			return errors.New("private artifact Windows device namespaces are not allowed")
		}
	}
	return nil
}

func validatePrivateArtifactWindowsComponents(components []string) error {
	if len(components) == 0 {
		return errors.New("private artifact Windows target path is invalid")
	}
	for _, component := range components {
		if err := validatePrivateArtifactWindowsLeaf(component); err != nil {
			return err
		}
	}
	return nil
}
