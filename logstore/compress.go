package logstore

import "github.com/klauspost/compress/zstd"

// zstdEnc and zstdDec are shared, stateless coders. EncodeAll and DecodeAll are
// safe for concurrent use, so a single instance of each serves all
// writes/reads.
//
// Both are configured for low, steady memory rather than throughput: by default
// the encoder and decoder spin up one set of (multi-MB) window/hash buffers per
// GOMAXPROCS, which dominates this process's resident memory even though each
// log record is tiny and compressed/decompressed one-shot. Pinning concurrency
// to 1 and enabling decoder low-memory mode keeps the footprint small; logging
// is off the request hot path, so serializing these is fine.
var (
	zstdEnc, _ = zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
	)
	zstdDec, _ = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
	)
)

func zstdCompress(b []byte) []byte {
	return zstdEnc.EncodeAll(b, nil)
}

func zstdDecompress(b []byte) ([]byte, error) {
	return zstdDec.DecodeAll(b, nil)
}
