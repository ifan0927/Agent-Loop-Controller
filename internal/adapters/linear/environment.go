package linear

import (
	"context"
	"errors"
	"os"
	"strings"
)

// EnvironmentCredentialSource reads an operator-provided token only at runtime.
// The token is never persisted or included in controller results.
type EnvironmentCredentialSource struct {
	Variable string
}

func (s EnvironmentCredentialSource) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ref != EnvironmentCredentialSourceRef {
		return "", errors.New("Linear credential source is unavailable")
	}
	if strings.TrimSpace(s.Variable) == "" {
		return "", errors.New("Linear credential environment variable is not configured")
	}
	value, ok := os.LookupEnv(s.Variable)
	if !ok || strings.TrimSpace(value) == "" {
		return "", errors.New("Linear credentials are unavailable")
	}
	return value, nil
}

// Check reports only whether the configured environment variable is present.
// It deliberately does not return or otherwise inspect the credential value.
func (s EnvironmentCredentialSource) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(s.Variable) == "" {
		return errors.New("Linear credential source is unavailable")
	}
	value, ok := os.LookupEnv(s.Variable)
	if !ok || strings.TrimSpace(value) == "" {
		return errors.New("Linear credential source is unavailable")
	}
	return nil
}
