package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/synctv-org/synctv/internal/conf"
	"github.com/synctv-org/synctv/utils"
	"github.com/zijiren233/stream"
	"gorm.io/gorm"
)

type BilibiliVendor struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	Cookies   map[string]string `gorm:"not null;serializer:fastjson;type:text"`
	UserID    string            `gorm:"primaryKey;type:char(32)"`
	Backend   string            `gorm:"type:varchar(64)"`
}

func (b *BilibiliVendor) BeforeSave(_ *gorm.DB) error {
	key := []byte(b.UserID)
	for k, v := range b.Cookies {
		value, err := utils.CryptoToBase64([]byte(v), key)
		if err != nil {
			return err
		}

		b.Cookies[k] = value
	}

	return nil
}

func (b *BilibiliVendor) AfterSave(_ *gorm.DB) error {
	key := []byte(b.UserID)
	for k, v := range b.Cookies {
		value, err := utils.DecryptoFromBase64(v, key)
		if err != nil {
			return err
		}

		b.Cookies[k] = stream.BytesToString(value)
	}

	return nil
}

func (b *BilibiliVendor) AfterFind(tx *gorm.DB) error {
	return b.AfterSave(tx)
}

type AlistVendor struct {
	CreatedAt      time.Time
	UpdatedAt      time.Time
	UserID         string `gorm:"primaryKey;type:char(32)"`
	Backend        string `gorm:"type:varchar(64)"`
	ServerID       string `gorm:"primaryKey;type:char(32)"`
	Host           string `gorm:"not null;type:varchar(256)"`
	Username       string `gorm:"type:varchar(256)"`
	HashedPassword []byte
}

func GenAlistServerID(a *AlistVendor) {
	if a.ServerID == "" {
		a.ServerID = utils.SortUUIDWithUUID(uuid.NewMD5(uuid.NameSpaceURL, []byte(a.Host)))
	}
}

func (a *AlistVendor) BeforeSave(_ *gorm.DB) error {
	key := utils.GenCryptoKey(a.UserID)

	var err error
	if a.Host, err = utils.CryptoToBase64([]byte(a.Host), key); err != nil {
		return err
	}

	if a.Username, err = utils.CryptoToBase64([]byte(a.Username), key); err != nil {
		return err
	}

	if a.HashedPassword, err = utils.Crypto(a.HashedPassword, key); err != nil {
		return err
	}

	return nil
}

func (a *AlistVendor) AfterSave(_ *gorm.DB) error {
	key := utils.GenCryptoKey(a.UserID)

	host, err := utils.DecryptoFromBase64(a.Host, key)
	if err != nil {
		return err
	}

	a.Host = stream.BytesToString(host)

	username, err := utils.DecryptoFromBase64(a.Username, key)
	if err != nil {
		return err
	}

	a.Username = stream.BytesToString(username)

	hashedPassword, err := utils.Decrypto(a.HashedPassword, key)
	if err != nil {
		return err
	}

	a.HashedPassword = hashedPassword

	return nil
}

func (a *AlistVendor) AfterFind(tx *gorm.DB) error {
	return a.AfterSave(tx)
}

// ErrEmbyCredentialSecretUnset is returned by the Emby password key helper when
// no server-side credential secret is configured. Callers must treat the stored
// credentials as unavailable rather than falling back to a weaker key.
var ErrEmbyCredentialSecretUnset = errors.New("vendor credential secret is not configured")

type EmbyVendor struct {
	CreatedAt  time.Time
	UpdatedAt  time.Time
	UserID     string `gorm:"primaryKey;type:char(32)"`
	Backend    string `gorm:"type:varchar(64)"`
	ServerID   string `gorm:"primaryKey;type:char(32)"`
	Host       string `gorm:"not null;type:varchar(256)"`
	APIKey     string `gorm:"not null;type:varchar(256)"`
	EmbyUserID string `gorm:"type:varchar(32)"`
	Username   string `gorm:"type:varchar(256)"`

	// Password is the user's RAW Emby password, not a protocol-specific hash.
	//
	// Emby's /emby/Users/authenticatebyname endpoint requires the plaintext
	// password, so unlike AlistVendor.HashedPassword (which only lets an attacker
	// authenticate against Alist) this is a genuinely recoverable user credential.
	//
	// For that reason it is NOT encrypted with GenCryptoKey(ServerID) like Host,
	// APIKey and Username: ServerID lives in this very row, so a database dump
	// alone would be enough to recompute that key. The password key is instead
	// derived from the server-side conf.Conf.Vendor.CredentialSecret combined with
	// ServerID, so the database on its own is insufficient. See embyPasswordKey.
	Password []byte
}

// embyPasswordKey derives the AES key protecting EmbyVendor.Password from the
// server-side credential secret plus the row's ServerID, so that two servers'
// rows never share a key and a database dump alone cannot recompute it.
//
// If the secret is unset (config predating this feature, or explicitly blank) it
// returns ErrEmbyCredentialSecretUnset. Callers MUST NOT fall back to a
// row-derivable key: the feature degrades to "no stored credentials" instead of
// offering a false sense of security.
func embyPasswordKey(serverID string) ([]byte, error) {
	secret := ""
	if conf.Conf != nil {
		secret = conf.Conf.Vendor.CredentialSecret
	}

	if secret == "" {
		return nil, ErrEmbyCredentialSecretUnset
	}

	return utils.GenCryptoKey(secret + serverID), nil
}

func (e *EmbyVendor) BeforeSave(_ *gorm.DB) error {
	key := utils.GenCryptoKey(e.ServerID)

	var err error
	if e.Host, err = utils.CryptoToBase64(stream.StringToBytes(e.Host), key); err != nil {
		return err
	}

	if e.APIKey, err = utils.CryptoToBase64(stream.StringToBytes(e.APIKey), key); err != nil {
		return err
	}

	// Rows created before credentials were stored have an empty Username; leave
	// them untouched so they keep round-tripping through AfterFind.
	if e.Username != "" {
		if e.Username, err = utils.CryptoToBase64(stream.StringToBytes(e.Username), key); err != nil {
			return err
		}
	}

	if len(e.Password) != 0 {
		passwordKey, err := embyPasswordKey(e.ServerID)
		if err != nil {
			// No credential secret: drop the password rather than persist it under a
			// key that a database dump could recompute. Auto-refresh is disabled.
			e.Password = nil
			return nil
		}

		if e.Password, err = utils.Crypto(e.Password, passwordKey); err != nil {
			return err
		}
	}

	return nil
}

func (e *EmbyVendor) AfterSave(_ *gorm.DB) error {
	key := utils.GenCryptoKey(e.ServerID)

	host, err := utils.DecryptoFromBase64(e.Host, key)
	if err != nil {
		return err
	}

	e.Host = stream.BytesToString(host)

	apiKey, err := utils.DecryptoFromBase64(e.APIKey, key)
	if err != nil {
		return err
	}

	e.APIKey = stream.BytesToString(apiKey)

	// Empty Username means a binding created before credentials were stored.
	// utils.Decrypto rejects short input with "ciphertext too short", so an
	// unguarded decrypt here would make every pre-existing row unreadable.
	if e.Username != "" {
		username, err := utils.DecryptoFromBase64(e.Username, key)
		if err != nil {
			return err
		}

		e.Username = stream.BytesToString(username)
	}

	if len(e.Password) != 0 {
		passwordKey, err := embyPasswordKey(e.ServerID)
		if err != nil {
			// No credential secret: the stored password cannot be read. Treat the
			// credential as unavailable instead of failing the whole row.
			e.Password = nil
			return nil
		}

		password, err := utils.Decrypto(e.Password, passwordKey)
		if err != nil {
			return err
		}

		e.Password = password
	}

	return nil
}

func (e *EmbyVendor) AfterFind(tx *gorm.DB) error {
	return e.AfterSave(tx)
}
