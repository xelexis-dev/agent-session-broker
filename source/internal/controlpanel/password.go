package controlpanel

import "golang.org/x/crypto/argon2"

type passwordKDFParameters struct {
	time    uint32
	memory  uint32
	threads uint8
	keyLen  uint32
}

// Stored verifiers do not encode KDF parameters. Changing these defaults would
// invalidate existing passwords; tests pin this compatibility contract.
var productionPasswordKDF = passwordKDFParameters{time: 2, memory: 19 * 1024, threads: 1, keyLen: 32}
var passwordKDF = productionPasswordKDF

func derivePasswordKey(password, salt []byte) []byte {
	return argon2.IDKey(password, salt, passwordKDF.time, passwordKDF.memory, passwordKDF.threads, passwordKDF.keyLen)
}
