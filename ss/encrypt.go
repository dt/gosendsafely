package ss

import (
	"bytes"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// EncryptPGP encrypts data using symmetric PGP with AES-256.
// The passphrase should be serverSecret + keyCode.
func EncryptPGP(data []byte, passphrase string) ([]byte, error) {
	var buf bytes.Buffer
	config := &packet.Config{DefaultCipher: packet.CipherAES256}

	w, err := openpgp.SymmetricallyEncrypt(&buf, []byte(passphrase), nil, config)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
