package skillresources_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sr "github.com/contextlattice/gateway-go/internal/skillresources"
)

func TestStrictObjectContracts(t *testing.T) {
	base := string(wire(t, fixture()))
	// Each object in the wire schema is exercised independently, including the
	// resource object nested inside an array and decoded-key duplicate aliases.
	anchors := []string{
		`"manifest":`, `"schema_id":`, `"conditions":`, `"os":`,
		`"identity_sha256":`, `"path":"scripts/check.mjs","interpreter":`,
		`"source":`, `"path":"scripts/check.mjs","size_bytes":`,
	}
	for i, anchor := range anchors {
		t.Run(fmt.Sprintf("object_%d", i), func(t *testing.T) {
			for _, prefix := range []string{
				`"unrecognized_secret_marker":"do-not-echo-this",`,
				`"unrecognized_secret_marker":null,`,
			} {
				raw := strings.Replace(base, anchor, prefix+anchor, 1)
				if raw == base {
					t.Fatal("test did not change an object")
				}
				assertDecodeRejected(t, raw)
			}
		})
	}
	var objects map[string]any
	if err := json.Unmarshal([]byte(base), &objects); err != nil {
		t.Fatal(err)
	}
	var visit func(map[string]any, []string)
	visit = func(object map[string]any, route []string) {
		for key, value := range object {
			for _, mutation := range []string{"missing", "null", "case", "duplicate"} {
				t.Run(strings.Join(append(route, key, mutation), "/"), func(t *testing.T) {
					var copyObject map[string]any
					if err := json.Unmarshal([]byte(base), &copyObject); err != nil {
						t.Fatal(err)
					}
					current := copyObject
					for _, part := range route {
						if part == "resources" {
							current = current[part].([]any)[0].(map[string]any)
						} else {
							current = current[part].(map[string]any)
						}
					}
					switch mutation {
					case "missing":
						delete(current, key)
					case "null":
						current[key] = nil
					case "case":
						current[strings.ToUpper(key)] = current[key]
						delete(current, key)
					}
					raw, err := json.Marshal(copyObject)
					if err != nil {
						t.Fatal(err)
					}
					if mutation == "duplicate" {
						encoded, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						needle := `"` + key + `":`
						raw = bytes.Replace(raw, []byte(needle), []byte(needle+string(encoded)+","+needle), 1)
					}
					assertDecodeRejected(t, string(raw))
				})
			}
			if nested, ok := value.(map[string]any); ok {
				visit(nested, append(append([]string{}, route...), key))
			} else if key == "resources" {
				visit(value.([]any)[0].(map[string]any), []string{"resources"})
			}
		}
	}
	visit(objects, nil)
	assertDecodeRejected(t, strings.Replace(base, `"version":1`, `"version":1,"\u0076ersion":1`, 1))
}

func assertDecodeRejected(t *testing.T, raw string) {
	t.Helper()
	_, err := sr.Decode([]byte(raw), skill)
	if err == nil {
		t.Fatal("invalid JSON admitted")
	}
	if strings.Contains(err.Error(), "do-not-echo-this") || strings.Contains(err.Error(), "unrecognized_secret_marker") {
		t.Fatal("error disclosed payload content")
	}
	allowed := []error{sr.ErrJSON, sr.ErrLimit, sr.ErrManifest, sr.ErrPath, sr.ErrContent, sr.ErrDigest, sr.ErrEntrypoint}
	for _, known := range allowed {
		if errors.Is(err, known) {
			return
		}
	}
	t.Fatal("error did not use a fixed content-free code")
}

func TestMalformedWireAndDepth(t *testing.T) {
	base := string(wire(t, fixture()))
	for _, raw := range []string{
		"", " ", "null", "false", "[]", "{}", base + base, base + "x", "/*x*/" + base,
		strings.Replace(base, `"version":1`, `"version":1.0`, 1),
		strings.Replace(base, `"version":1`, `"version":1e0`, 1),
		strings.Replace(base, `"version":1`, `"version":99999999999999999999999999`, 1),
		strings.Replace(base, `"version":1`, `"version":"1"`, 1),
		strings.Replace(base, `"resources":[`, `"resources":[null,`, 1),
		strings.Replace(base, `"conditions":[`, `"conditions":[1,`, 1),
		strings.Replace(base, `"safe condition"`, "\"bad\xff\"", 1),
		strings.Replace(base, `"safe condition"`, `"\uD800"`, 1),
		strings.Replace(base, `"safe condition"`, `"\uDFFF"`, 1),
		strings.Replace(base, `"safe condition"`, `"\uD800\u0041"`, 1),
		strings.Replace(base, `"safe condition"`, `"\uD800\uD800"`, 1),
		strings.Replace(base, `"safe condition"`, `"\uD800x\uDC00"`, 1),
		strings.Replace(base, `"safe condition"`, `"\u0000"`, 1),
		strings.Replace(base, `"safe condition"`, `"\x00"`, 1),
	} {
		assertDecodeRejected(t, raw)
	}
	deep := strings.Repeat("[", sr.MaxJSONDepth+1) + "0" + strings.Repeat("]", sr.MaxJSONDepth+1)
	if _, err := sr.Decode([]byte(deep), skill); !errors.Is(err, sr.ErrLimit) {
		t.Fatalf("depth limit not applied: %v", err)
	}
	for _, value := range []string{`"\uD83D\uDE00"`, `"\uFFFD"`, `"literal \\uD800"`, `"quoted \\"`, `"[[[[[[[[[[{}}}}}}}}}}"`} {
		raw := strings.Replace(base, `"safe condition"`, value, 1)
		if _, err := sr.Decode([]byte(raw), skill); err != nil {
			t.Fatalf("valid Unicode/string escaped content rejected: %v", err)
		}
	}
}

func TestContentEncodingIsStrictBase64(t *testing.T) {
	bundle := fixture()
	bundle.Resources[0] = resource(bundle.Resources[0].Path, []byte("a"))
	base := string(wire(t, bundle))
	for _, value := range []string{`"YR=="`, `"YQ==\n"`, `"YQ"`, `"-w=="`, `"!!!!"`, `[97]`, `null`, `97`} {
		assertDecodeRejected(t, strings.Replace(base, `"content":"YQ=="`, `"content":`+value, 1))
	}
	bundle.Resources[0] = resource(bundle.Resources[0].Path, []byte{})
	if _, err := sr.Decode(wire(t, bundle), skill); err != nil {
		t.Fatal("explicit empty bytes rejected:", err)
	}
}

func TestCanonicalObjectOrderAndEscapes(t *testing.T) {
	bundle := fixture()
	bundle.Manifest.Applicability.Conditions[0] = "<>& \u00e9 \U0001f600"
	want := digest(t, bundle, skill)
	var object map[string]any
	if err := json.Unmarshal(wire(t, bundle), &object); err != nil {
		t.Fatal(err)
	}
	// Maps emit lexical field order, unlike the fixed struct encoding. Escaping
	// changes transport only; decoded semantic strings and bytes remain exact.
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(`\u003c`), []byte("<"))
	raw = bytes.ReplaceAll(raw, []byte(`\u003e`), []byte(">"))
	raw = bytes.ReplaceAll(raw, []byte(`\u0026`), []byte("&"))
	decoded, err := sr.Decode(raw, skill)
	if err != nil || digest(t, decoded, skill) != want {
		t.Fatal("JSON ordering or escaping changed identity:", err)
	}
	first := fixture()
	first.Manifest.Entrypoint.Arguments = []string{"first", "second"}
	second := fixture()
	second.Manifest.Entrypoint.Arguments = []string{"second", "first"}
	if digest(t, first, skill) == digest(t, second, skill) {
		t.Fatal("argv order lost")
	}
}

func TestMaximumMetadataAndPath(t *testing.T) {
	bundle := fixture()
	fill := func() []string {
		values := make([]string, sr.MaxMetadataItems)
		for i := range values {
			values[i] = strings.Repeat("<", sr.MaxMetadataBytes)
		}
		return values
	}
	bundle.Manifest.Applicability.Conditions = fill()
	bundle.Manifest.Applicability.Exclusions = fill()
	bundle.Manifest.Applicability.FailureModes = fill()
	bundle.Manifest.Rollback.Steps = fill()
	bundle.Manifest.Entrypoint.Arguments = fill()
	bundle.Manifest.Entrypoint.Interpreter.Arguments = fill()
	for len(bundle.Resources) < sr.MaxResources {
		bundle.Resources = append(bundle.Resources, resource(fmt.Sprintf("references/n%d", len(bundle.Resources)), []byte{}))
	}
	for i := range bundle.Resources {
		bundle.Resources[i] = resource(bundle.Resources[i].Path, bytes.Repeat([]byte("x"), sr.MaxResourceBytes))
	}
	raw, err := sr.CanonicalJSON(bundle, skill)
	if err != nil || len(raw) > sr.MaxJSONBytes {
		t.Fatal("maximum escaped metadata plus resources exceeds wire contract", err)
	}
	if _, err := sr.Decode(raw, skill); err != nil {
		t.Fatal("maximum canonical bundle did not decode:", err)
	}
	bundle = fixture()
	name := "scripts/" + strings.Repeat("a", 64) + "/" + strings.Repeat("b", 64) + "/" + strings.Repeat("c", 64) + "/" + strings.Repeat("d", 37)
	if len(name) != sr.MaxPathBytes {
		t.Fatal("incorrect boundary fixture")
	}
	bundle.Resources[1].Path = name
	if err := sr.Validate(bundle, skill); err != nil {
		t.Fatal("exact path boundary rejected:", err)
	}
	bundle.Resources[1].Path += "e"
	if err := sr.Validate(bundle, skill); !errors.Is(err, sr.ErrPath) {
		t.Fatal("path boundary overflow admitted")
	}
}

func FuzzDecode(f *testing.F) {
	raw, _ := json.Marshal(fixture())
	f.Add(raw)
	f.Add([]byte(`{"manifest":{"version":1,"version":1}}`))
	f.Add([]byte(`{"x":"\uD800"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		bundle, err := sr.Decode(data, skill)
		if err != nil {
			return
		}
		canonical, err := sr.CanonicalJSON(bundle, skill)
		if err != nil {
			t.Fatal("accepted bundle cannot encode:", err)
		}
		again, err := sr.Decode(canonical, skill)
		if err != nil || digest(t, again, skill) != digest(t, bundle, skill) {
			t.Fatal("accepted bundle not stable:", err)
		}
	})
}
