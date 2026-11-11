package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestTelemetryCompressionPreservesExactContent(t *testing.T) {
	inputs := []string{"", "one line\n", "\x00\xff\xfe\r\n\t", strings.Repeat("payload: 0123456789\n", 4096)}
	for _, codec := range []string{"", "zstd", "gzip"} {
		for _, level := range []int{1, 3, 7, 12} {
			for index, input := range inputs {
				t.Run(fmt.Sprintf("%s/%d/%d", codec, level, index), func(t *testing.T) {
					encoded, actual, err := compressTelemetryContent(input, codec, level)
					if err != nil {
						t.Fatal(err)
					}
					wantCodec := codec
					if wantCodec == "" {
						wantCodec = "zstd"
					}
					if actual != wantCodec {
						t.Fatalf("codec silently changed: got %q, want %q", actual, wantCodec)
					}
					var decoded []byte
					if actual == "zstd" {
						reader, openErr := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderConcurrency(1))
						if openErr != nil {
							t.Fatal(openErr)
						}
						defer reader.Close()
						decoded, err = reader.DecodeAll(encoded, nil)
					} else {
						reader, openErr := gzip.NewReader(bytes.NewReader(encoded))
						if openErr != nil {
							t.Fatal(openErr)
						}
						defer reader.Close()
						decoded, err = io.ReadAll(io.LimitReader(reader, int64(len(input)+1)))
					}
					if err != nil || string(decoded) != input {
						t.Fatalf("content changed for %s: error=%v input=%d decoded=%d", actual, err, len(input), len(decoded))
					}
				})
			}
		}
	}
}
