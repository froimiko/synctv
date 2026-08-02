package model

import (
	"testing"

	"github.com/synctv-org/synctv/internal/conf"
)

// withCredentialSecret installs a credential secret for the duration of a test
// and restores whatever was there before.
func withCredentialSecret(t *testing.T, secret string) {
	t.Helper()

	previous := conf.Conf
	t.Cleanup(func() { conf.Conf = previous })

	c := conf.DefaultConfig()
	c.Vendor.CredentialSecret = secret
	conf.Conf = c
}

func TestEmbyVendorRoundTripsCredentials(t *testing.T) {
	withCredentialSecret(t, "test-credential-secret")

	const (
		host     = "https://emby.example"
		apiKey   = "session-token"
		username = "alice"
		password = "s3cr3t"
	)

	v := &EmbyVendor{
		UserID:     "user-id",
		ServerID:   "server-id",
		Host:       host,
		APIKey:     apiKey,
		EmbyUserID: "emby-user",
		Username:   username,
		Password:   []byte(password),
	}

	if err := v.BeforeSave(nil); err != nil {
		t.Fatalf("BeforeSave: %v", err)
	}

	// Everything sensitive must actually be transformed on the way out.
	if v.Host == host || v.APIKey == apiKey || v.Username == username {
		t.Fatal("host, api key and username must not be persisted in plaintext")
	}

	if string(v.Password) == password {
		t.Fatal("password must not be persisted in plaintext")
	}

	if err := v.AfterSave(nil); err != nil {
		t.Fatalf("AfterSave: %v", err)
	}

	if v.Host != host || v.APIKey != apiKey {
		t.Fatalf("host/apiKey did not round-trip: %q %q", v.Host, v.APIKey)
	}

	if v.Username != username {
		t.Fatalf("username = %q, want %q", v.Username, username)
	}

	if string(v.Password) != password {
		t.Fatalf("password did not round-trip")
	}
}

func TestEmbyVendorWithoutCredentialsStaysReadable(t *testing.T) {
	// Bindings created before this feature have no username/password. They must
	// keep round-tripping, otherwise AfterFind would fail on every existing row
	// and make all of them unreadable.
	withCredentialSecret(t, "test-credential-secret")

	v := &EmbyVendor{
		UserID:     "user-id",
		ServerID:   "server-id",
		Host:       "https://emby.example",
		APIKey:     "session-token",
		EmbyUserID: "emby-user",
	}

	if err := v.BeforeSave(nil); err != nil {
		t.Fatalf("BeforeSave: %v", err)
	}

	if err := v.AfterSave(nil); err != nil {
		t.Fatalf("AfterSave: %v", err)
	}

	if v.Host != "https://emby.example" || v.APIKey != "session-token" {
		t.Fatal("pre-existing row did not survive the hooks")
	}

	if v.Username != "" || len(v.Password) != 0 {
		t.Fatal("absent credentials must stay absent")
	}
}

func TestEmbyVendorDropsPasswordWithoutCredentialSecret(t *testing.T) {
	// With no server-side secret there is no key that a database dump could not
	// recompute, so the password must be dropped rather than stored under a
	// row-derivable key.
	withCredentialSecret(t, "")

	v := &EmbyVendor{
		UserID:     "user-id",
		ServerID:   "server-id",
		Host:       "https://emby.example",
		APIKey:     "session-token",
		EmbyUserID: "emby-user",
		Username:   "alice",
		Password:   []byte("s3cr3t"),
	}

	if err := v.BeforeSave(nil); err != nil {
		t.Fatalf("BeforeSave: %v", err)
	}

	if len(v.Password) != 0 {
		t.Fatal("password must be dropped when no credential secret is configured")
	}
}
