package skillresources

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decode admits exactly one bounded JSON bundle with exact, case-sensitive
// field names. Every field is required; unknown/duplicate keys and nulls fail.
// On failure it returns a zero Bundle and a content-free error.
func Decode(data, skillMarkdown []byte) (Bundle, error) {
	if len(data) > MaxJSONBytes {
		return Bundle{}, ErrLimit
	}
	if exceedsJSONDepth(data) {
		return Bundle{}, ErrLimit
	}
	if !utf8.Valid(data) || !json.Valid(data) || !validUnicodeEscapes(data) {
		return Bundle{}, ErrJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkValue(decoder, reflect.TypeOf(Bundle{}), 1); err != nil {
		return Bundle{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Bundle{}, ErrJSON
	}
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return Bundle{}, ErrJSON
	}
	if err := Validate(bundle, skillMarkdown); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func exceedsJSONDepth(data []byte) bool {
	depth, inString := 0, false
	for i := 0; i < len(data); i++ {
		if inString && data[i] == '\\' {
			i++
			continue
		}
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch data[i] {
		case '{', '[':
			depth++
			if depth > MaxJSONDepth {
				return true
			}
		case '}', ']':
			depth--
		}
	}
	return false
}

// CanonicalJSON verifies the bundle and returns its canonical wire encoding.
// Only resource order is normalized, by bytewise path order. All other lists,
// strings and content bytes retain their exact values; inputs are not mutated.
func CanonicalJSON(bundle Bundle, skillMarkdown []byte) ([]byte, error) {
	if err := Validate(bundle, skillMarkdown); err != nil {
		return nil, err
	}
	bundle.Resources = append([]Resource(nil), bundle.Resources...)
	sort.Slice(bundle.Resources, func(i, j int) bool { return bundle.Resources[i].Path < bundle.Resources[j].Path })
	raw, err := json.Marshal(bundle)
	if err != nil {
		return nil, ErrJSON
	}
	if len(raw) > MaxJSONBytes {
		return nil, ErrLimit
	}
	return raw, nil
}

// Digest hashes CanonicalJSON, including all manifest fields and resource
// bytes, and returns sha256:<lowercase hex>. It is not execution authority.
func Digest(bundle Bundle, skillMarkdown []byte) (string, error) {
	raw, err := CanonicalJSON(bundle, skillMarkdown)
	if err != nil {
		return "", err
	}
	return SHA256(raw), nil
}

// The typed preflight closes encoding/json's duplicate-key, case-folded-key,
// and null-to-zero-value acceptance without exposing decoder error contents.
func checkValue(decoder *json.Decoder, expected reflect.Type, depth int) error {
	if depth > MaxJSONDepth {
		return ErrLimit
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return ErrJSON
	}
	switch expected.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return ErrJSON
		}
		seen := make(map[string]bool, expected.NumField())
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok || seen[key] {
				return ErrJSON
			}
			seen[key] = true
			var fieldType reflect.Type
			for i := 0; i < expected.NumField(); i++ {
				field := expected.Field(i)
				if field.Tag.Get("json") == key {
					fieldType = field.Type
					break
				}
			}
			if fieldType == nil {
				return ErrJSON
			}
			if err := checkValue(decoder, fieldType, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') || len(seen) != expected.NumField() {
			return ErrJSON
		}
	case reflect.Slice:
		if expected.Elem().Kind() == reflect.Uint8 {
			value, ok := token.(string)
			if !ok || len(value) > base64.StdEncoding.EncodedLen(MaxResourceBytes) {
				return ErrJSON
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(value)
			if err != nil || base64.StdEncoding.EncodeToString(decoded) != value {
				return ErrJSON
			}
			return nil
		}
		if token != json.Delim('[') {
			return ErrJSON
		}
		limit := MaxMetadataItems
		if expected.Elem().Kind() == reflect.Struct {
			limit = MaxResources
		}
		count := 0
		for decoder.More() {
			count++
			if count > limit {
				return ErrLimit
			}
			if err := checkValue(decoder, expected.Elem(), depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrJSON
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return ErrJSON
		}
	case reflect.Int:
		value, ok := token.(json.Number)
		if !ok || strings.ContainsAny(string(value), ".eE") {
			return ErrJSON
		}
		if _, err := strconv.ParseInt(string(value), 10, strconv.IntSize); err != nil {
			return ErrJSON
		}
	default:
		return ErrJSON
	}
	return nil
}

// json.Valid permits unpaired UTF-16 escapes, which Unmarshal replaces with
// U+FFFD. Reject those escapes instead of silently changing identity material.
// Syntax and raw UTF-8 validity must be checked before calling this function.
func validUnicodeEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if data[i] != 'u' {
			continue
		}
		value, _ := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value < 0xd800 || value > 0xdbff {
			continue
		}
		if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
