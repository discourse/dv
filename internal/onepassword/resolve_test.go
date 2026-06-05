package onepassword

import (
	"reflect"
	"testing"
)

func TestOpReadArgsPlainReference(t *testing.T) {
	got := opReadArgs("op://Vault/Item/field")
	want := []string{"read", "op://Vault/Item/field"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opReadArgs = %#v, want %#v", got, want)
	}
}

func TestOpReadArgsAccountQuery(t *testing.T) {
	got := opReadArgs("op://Everybody/Snorlax LLM/credential?account=discourse.1password.com")
	want := []string{"read", "--account", "discourse.1password.com", "op://Everybody/Snorlax LLM/credential"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opReadArgs = %#v, want %#v", got, want)
	}
}

func TestOpReadArgsPreservesOtherQuery(t *testing.T) {
	got := opReadArgs("op://Vault/Item/field?account=example.1password.com&foo=bar")
	want := []string{"read", "--account", "example.1password.com", "op://Vault/Item/field?foo=bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opReadArgs = %#v, want %#v", got, want)
	}
}
