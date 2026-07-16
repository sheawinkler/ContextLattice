package main

import (
	"os"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"
)

const defaultContextPackTokenizerEncoding = "o200k_base"

type tokenCountResult struct {
	Tokens           int
	Method           string
	CalibrationGrade string
	Encoding         string
	TokenizerExact   bool
	Warning          string
}

var contextPackTokenizerCache = struct {
	sync.Mutex
	tokenizers map[string]*tiktoken.Tiktoken
	errors     map[string]string
}{
	tokenizers: map[string]*tiktoken.Tiktoken{},
	errors:     map[string]string{},
}

func init() {
	// Token accounting must remain exact in offline and network-restricted runtimes.
	tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
}

func contextPackEstimateTokens(text string) int {
	return contextPackCountTokens(text).Tokens
}

func contextPackTokenCountMetadata() tokenCountResult {
	result := contextPackCountTokens("contextlattice tokenizer probe")
	result.Tokens = 0
	return result
}

func contextPackCountTokens(text string) tokenCountResult {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		trimmed = " "
	}
	encoding := contextPackTokenizerEncoding()
	tokenizer, errText := contextPackTokenizerForEncoding(encoding)
	if tokenizer != nil {
		return tokenCountResult{
			Tokens:           maxInt(1, len(tokenizer.EncodeOrdinary(trimmed))),
			Method:           "tiktoken",
			CalibrationGrade: "tokenizer_exact",
			Encoding:         encoding,
			TokenizerExact:   true,
		}
	}
	return tokenCountResult{
		Tokens:           maxInt(1, len(trimmed)/4),
		Method:           "chars_div_4",
		CalibrationGrade: "sampled_pack_estimate",
		Encoding:         "",
		TokenizerExact:   false,
		Warning:          errText,
	}
}

func contextPackTokenizerEncoding() string {
	if raw := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_TOKENIZER_ENCODING")); raw != "" {
		return raw
	}
	if raw := strings.TrimSpace(os.Getenv("GO_CONTEXTLATTICE_TOKENIZER_ENCODING")); raw != "" {
		return raw
	}
	model := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_TOKENIZER_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	}
	if model != "" && !strings.Contains(model, "/") && !strings.Contains(model, "\\") {
		if encoding, ok := tiktoken.MODEL_TO_ENCODING[model]; ok {
			return encoding
		}
		for prefix, encoding := range tiktoken.MODEL_PREFIX_TO_ENCODING {
			if strings.HasPrefix(model, prefix) {
				return encoding
			}
		}
	}
	return defaultContextPackTokenizerEncoding
}

func contextPackTokenizerForEncoding(encoding string) (*tiktoken.Tiktoken, string) {
	encoding = strings.TrimSpace(encoding)
	if encoding == "" {
		encoding = defaultContextPackTokenizerEncoding
	}
	contextPackTokenizerCache.Lock()
	if tokenizer := contextPackTokenizerCache.tokenizers[encoding]; tokenizer != nil {
		contextPackTokenizerCache.Unlock()
		return tokenizer, ""
	}
	if errText := contextPackTokenizerCache.errors[encoding]; errText != "" {
		contextPackTokenizerCache.Unlock()
		return nil, errText
	}
	contextPackTokenizerCache.Unlock()

	tokenizer, err := tiktoken.GetEncoding(encoding)
	contextPackTokenizerCache.Lock()
	defer contextPackTokenizerCache.Unlock()
	if err != nil {
		errText := err.Error()
		contextPackTokenizerCache.errors[encoding] = errText
		return nil, errText
	}
	contextPackTokenizerCache.tokenizers[encoding] = tokenizer
	return tokenizer, ""
}
