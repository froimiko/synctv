package conf

import (
	"github.com/synctv-org/synctv/utils"
)

//nolint:tagliatelle
type VendorConfig struct {
	// CredentialSecret is the server-side secret used to derive the AES key that
	// protects recoverable vendor credentials (currently the Emby password).
	//
	// It deliberately lives in server config instead of the database so that a
	// leaked database dump alone is not enough to recover the plaintext
	// credential. Changing or losing it invalidates every stored Emby login;
	// affected users simply have to rebind.
	CredentialSecret string `env:"VENDOR_CREDENTIAL_SECRET" yaml:"credential_secret" hc:"secret used to encrypt stored vendor credentials, changing it invalidates saved emby logins"`
}

func DefaultVendorConfig() VendorConfig {
	return VendorConfig{
		CredentialSecret: utils.RandString(32),
	}
}
