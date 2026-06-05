package onepassword

import (
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// IsReference checks if a string is a 1Password reference (op://...).
func IsReference(s string) bool {
	return strings.HasPrefix(s, "op://")
}

// Read fetches a secret from 1Password using `op read <reference>`.
//
// Secret references may include an account selector as a query parameter, e.g.
// `op://Vault/Item/field?account=example.1password.com`. The 1Password CLI
// expects that selector as `--account`, so Read translates it before invoking
// `op read`.
//
// Returns an error if the op CLI is not available or the reference is invalid.
func Read(reference string) (string, error) {
	if !IsReference(reference) {
		return "", fmt.Errorf("not a 1Password reference: %s", reference)
	}

	args := opReadArgs(reference)
	cmd := exec.Command("op", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("op read failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("op read failed: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

func opReadArgs(reference string) []string {
	ref, account := splitAccountSelector(reference)
	args := []string{"read"}
	if account != "" {
		args = append(args, "--account", account)
	}
	return append(args, ref)
}

func splitAccountSelector(reference string) (string, string) {
	queryStart := strings.Index(reference, "?")
	if queryStart == -1 {
		return reference, ""
	}

	base := reference[:queryStart]
	rawQuery := reference[queryStart+1:]
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return reference, ""
	}

	account := strings.TrimSpace(query.Get("account"))
	if account == "" {
		return reference, ""
	}

	query.Del("account")
	if encoded := query.Encode(); encoded != "" {
		base += "?" + encoded
	}
	return base, account
}

// CLIAvailable checks if the 1Password CLI is installed and accessible.
func CLIAvailable() bool {
	_, err := exec.LookPath("op")
	return err == nil
}

// ResolveValue resolves a single value, fetching from 1Password if it's a reference.
// Returns the resolved value and whether it was from 1Password.
func ResolveValue(value interface{}) (interface{}, bool, error) {
	str, ok := value.(string)
	if !ok {
		return value, false, nil
	}

	if !IsReference(str) {
		return value, false, nil
	}

	resolved, err := Read(str)
	if err != nil {
		return nil, true, err
	}

	return resolved, true, nil
}

// ResolveSettings resolves all 1Password references in a settings map.
// Returns a new map with resolved values and a map of which keys were from 1Password.
func ResolveSettings(settings map[string]interface{}) (map[string]interface{}, map[string]bool, error) {
	resolved := make(map[string]interface{})
	fromOP := make(map[string]bool)
	var errs []string

	for key, value := range settings {
		resolvedValue, wasOP, err := ResolveValue(value)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		resolved[key] = resolvedValue
		fromOP[key] = wasOP
	}

	if len(errs) > 0 {
		return resolved, fromOP, fmt.Errorf("failed to resolve some 1Password references:\n  %s", strings.Join(errs, "\n  "))
	}

	return resolved, fromOP, nil
}
