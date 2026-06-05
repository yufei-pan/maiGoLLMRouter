package logstore

import "github.com/klauspost/compress/zstd"

// zstdEnc and zstdDec are shared, stateless coders. EncodeAll and DecodeAll are
// safe for concurrent use, so a single instance of each serves all writes/reads.
var (
	zstdEnc, _ = zstd.NewWriter(nil)
	zstdDec, _ = zstd.NewReader(nil)
)

func zstdCompress(b []byte) []byte {
	return zstdEnc.EncodeAll(b, nil)
}

func zstdDecompress(b []byte) ([]byte, error) {
	return zstdDec.DecodeAll(b, nil)
}
