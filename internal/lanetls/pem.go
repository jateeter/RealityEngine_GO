package lanetls

import (
	"crypto/x509"
	"encoding/pem"
)

// pemBlock is the first block of the given type, or nil.
func pemBlock(data []byte, kind string) []byte {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil
		}
		if block.Type == kind {
			return block.Bytes
		}
	}
}

// pemCerts is every certificate in a PEM bundle.
func pemCerts(data []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			out = append(out, c)
		}
	}
}
