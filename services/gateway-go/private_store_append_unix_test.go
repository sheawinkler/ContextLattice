//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOwnerOnlyAppendIsAtomicAcrossConcurrentDescriptorsUnix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "append.ndjson")
	const writers = 8
	const rowsPerWriter = 64
	var group sync.WaitGroup
	group.Add(writers)
	for writer := 0; writer < writers; writer++ {
		writer := writer
		go func() {
			defer group.Done()
			file, err := openOwnerOnlyAppend(path, true)
			if err != nil {
				t.Errorf("open append descriptor %d: %v", writer, err)
				return
			}
			defer file.Close()
			for row := 0; row < rowsPerWriter; row++ {
				line := []byte(fmt.Sprintf("%d:%d:%s\n", writer, row, "append-regression"))
				if _, err := file.Write(line); err != nil {
					t.Errorf("append row %d/%d: %v", writer, row, err)
					return
				}
			}
		}()
	}
	group.Wait()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) != 3 || parts[2] != "append-regression" {
			t.Fatalf("concurrent append produced a truncated row: %q", scanner.Text())
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if want := writers * rowsPerWriter; count != want {
		t.Fatalf("concurrent append row count=%d want=%d", count, want)
	}
}
