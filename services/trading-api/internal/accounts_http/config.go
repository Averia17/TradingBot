package accounts_http

import (
	"errors"
	"io"
	"os"
	"strings"
)

const maxTokenLength = 512

// InternalToken reads the paper service credential from an environment value
// or a mounted file. A caller with no configured HTTP dependency may omit it.
func InternalToken() (string, error) {
	return readToken("TRADINGBOT_PAPER_INTERNAL_TOKEN", "TRADINGBOT_PAPER_INTERNAL_TOKEN_FILE")
}

// OutboxToken is scoped to delivery reads and acknowledgements.
func OutboxToken() (string, error) {
	return readToken("TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN", "TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN_FILE")
}

func readToken(env, fileEnv string) (string, error) {
	token := os.Getenv(env)
	file := os.Getenv(fileEnv)
	if token != "" && file != "" {
		return "", errors.New("configure only one paper internal token source")
	}
	if file != "" {
		input, err := os.Open(file)
		if err != nil {
			return "", errors.New("read paper internal token file")
		}
		defer input.Close()
		raw, err := io.ReadAll(io.LimitReader(input, maxTokenLength+3))
		if err != nil || len(raw) >= maxTokenLength+3 {
			return "", errors.New("read paper internal token file")
		}
		token = strings.TrimSpace(string(raw))
		if token == "" {
			return "", errors.New("paper internal token file is empty")
		}
	}
	if token != "" && !validToken(token) {
		return "", errors.New("paper internal token must contain 32 to 512 non-space ASCII characters")
	}
	return token, nil
}

func validToken(token string) bool {
	if len(token) < 32 || len(token) > maxTokenLength {
		return false
	}
	for _, c := range token {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}
