// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ategcs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/internal/ateerrors"
	"github.com/klauspost/compress/zstd"
	"go.opentelemetry.io/otel"
)

var tracer = otel.Tracer("ategcs")

type ObjectStorage interface {
	GetObject(ctx context.Context, bucket, object string) (io.ReadCloser, error)
	PutObject(ctx context.Context, bucket, object string, reader io.Reader) error
}

func FetchFromGCS(ctx context.Context, client ObjectStorage, gsURL string) ([]byte, error) {
	ctx, span := tracer.Start(ctx, "fetchFromGCS")
	defer span.End()

	bucket, object, err := parseGCSURL(gsURL)
	if err != nil {
		return nil, fmt.Errorf("%w: while parsing url: %w", ateerrors.ReasonInvalidObjectURL, err)
	}

	rc, err := client.GetObject(ctx, bucket, object)
	if err != nil {
		return nil, fmt.Errorf("while getting object bucket=%q object=%q: %w", bucket, object, err)
	}
	defer rc.Close()

	content, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("while reading all content: %w", err)
	}

	return content, nil
}

// Open streams the object at gsURL; the caller must Close the returned reader.
// Unlike FetchFromGCS it does not buffer the whole object in memory.
func Open(ctx context.Context, client ObjectStorage, gsURL string) (io.ReadCloser, error) {
	bucket, object, err := parseGCSURL(gsURL)
	if err != nil {
		return nil, fmt.Errorf("%w: while parsing url: %w", ateerrors.ReasonInvalidObjectURL, err)
	}
	rc, err := client.GetObject(ctx, bucket, object)
	if err != nil {
		return nil, fmt.Errorf("while getting object bucket=%q object=%q: %w", bucket, object, err)
	}
	return rc, nil
}

// SendBytesToGCS uploads the given bytes (uncompressed) to gsURL. Intended for
// small objects such as the snapshot manifest.
func SendBytesToGCS(ctx context.Context, client ObjectStorage, gsURL string, content []byte) error {
	ctx, span := tracer.Start(ctx, "sendBytesToGCS")
	defer span.End()

	bucket, object, err := parseGCSURL(gsURL)
	if err != nil {
		return fmt.Errorf("%w: while parsing url: %w", ateerrors.ReasonInvalidObjectURL, err)
	}
	if err := client.PutObject(ctx, bucket, object, bytes.NewReader(content)); err != nil {
		return fmt.Errorf("while putting object bucket=%q object=%q: %w", bucket, object, err)
	}
	return nil
}

// UploadStats reports the byte counts of one compressed upload, so a caller
// can meter the transfer at each layer: the file as it appears on disk, the
// bytes actually read, and the bytes that crossed the network.
type UploadStats struct {
	// LogicalBytes is the apparent size of the source, holes included.
	LogicalBytes int64
	// PopulatedBytes is the non-hole bytes actually read and compressed;
	// equal to LogicalBytes for a dense source.
	PopulatedBytes int64
	// WireBytes is the compressed bytes handed to the object store.
	WireBytes int64
	// Sparse is true when the sparse-extent format was used.
	Sparse bool
}

// DownloadStats mirrors UploadStats for a download: the logical size restored,
// the non-hole bytes actually written, and the compressed bytes fetched.
type DownloadStats struct {
	// LogicalBytes is the logical size written locally (the original image size).
	LogicalBytes int64
	// PopulatedBytes is the non-hole bytes actually written locally.
	PopulatedBytes int64
	// WireBytes is the compressed bytes read from the object store.
	WireBytes int64
	// Sparse is true when the object used the sparse-extent format.
	Sparse bool
}

// countingWriter counts the bytes written through it: the compressed (wire)
// bytes handed to the object store. The count is atomic so the range-parallel
// upload's part writers can share one counter.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n.Add(int64(n))
	return n, err
}

// countingReader counts the bytes read through it: the compressed (wire)
// bytes fetched from the object store.
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

func SendLocalFileToGCSWithZstd(ctx context.Context, client ObjectStorage, gsURL string, localFilePath string) (stats UploadStats, err error) {
	ctx, span := tracer.Start(ctx, "sendLocalFileToGCSWithZstd")
	defer span.End()

	localFile, err := os.Open(localFilePath)
	if err != nil {
		return UploadStats{}, fmt.Errorf("while opening %q: %w", localFilePath, err)
	}
	defer func() {
		if closeErr := localFile.Close(); closeErr != nil {
			if err == nil {
				err = closeErr
			} else {
				slog.InfoContext(ctx, "Dropped error from closing localFile", slog.String("localFile", localFilePath), slog.Any("err", err))
			}
		}
	}()

	stats, err = sendZstd(ctx, client, gsURL, localFile)
	if err != nil {
		return UploadStats{}, fmt.Errorf("in sendZstd: %w", err)
	}

	return stats, nil
}

// sparseFilePutter marks a backend that can upload a sparse FILE directly, splitting
// compression and upload by file range instead of piping one compressed stream into a
// part splitter. Reports errSparseTooSmall when the file is not worth splitting, in
// which case the caller falls back to the streaming path.
type sparseFilePutter interface {
	PutSparseFile(ctx context.Context, bucket, object string, f *os.File) (UploadStats, error)
}

// streamingPutter marks an ObjectStorage whose PutObject accepts a non-seekable
// streaming body without buffering (e.g. GCS): implementing the interface is the
// signal, so the marker method is never called. See gcsClient.
type streamingPutter interface{ supportsStreamingPut() }

// writeContentResult reports what writeContent compressed.
type writeContentResult struct {
	// logicalBytes is the total logical size of the source, including the holes
	// for a sparse file.
	logicalBytes int64
	// populatedBytes is the count of bytes actually read + compressed: the non-hole
	// (resident) set for the sparse-extent format, == logicalBytes for a plain stream.
	populatedBytes int64
	// sparse is true when the sparse-extent format was used (the source was a file).
	sparse bool
}

// writeContent compresses content to out, choosing the sparse-extent format for a
// seekable *os.File (compress only the populated extents, skip the holes) or a
// plain zstd stream otherwise. It touches only io, so it is unit-testable without
// an object store, and is shared by the buffered and streaming upload paths.
func writeContent(out io.Writer, content io.Reader) (writeContentResult, error) {
	if f, ok := content.(*os.File); ok {
		logical, populated, err := writeSparseZstd(out, f)
		if err != nil {
			return writeContentResult{}, err
		}
		return writeContentResult{logicalBytes: logical, populatedBytes: populated, sparse: true}, nil
	}
	logical, err := plainZstd(out, content)
	if err != nil {
		return writeContentResult{}, err
	}
	return writeContentResult{logicalBytes: logical, populatedBytes: logical}, nil
}

// sendZstd zstd-compresses content and uploads it to gsURL.
//
// The snapshot memory-ranges is the large object here (the whole guest RAM image,
// mostly zero) on the SUSPEND critical path, so we compress with SpeedFastest across
// all CPUs — high-ratio levels scan the multi-GiB image far slower for little size
// gain on near-zero data, and the decoder auto-detects the level so restore + older
// snapshots are unaffected.
//
// Upload strategy depends on the backend:
//   - Streaming backends (GCS) accept a non-seekable body, so we pipe the compressor
//     straight into PutObject: the compress overlaps the network PUT and we never
//     stage the ~100MiB compressed payload to a temp file.
//   - S3/rustfs PutObject hands the body to the AWS SDK, which needs a seekable body
//     to sign + set Content-Length (a non-seekable pipe hangs there), so we compress
//     to a SEEKABLE temp file first.
func sendZstd(ctx context.Context, client ObjectStorage, gsURL string, content io.Reader) (UploadStats, error) {
	bucket, object, err := parseGCSURL(gsURL)
	if err != nil {
		return UploadStats{}, fmt.Errorf("while parsing URL: %w", err)
	}
	tStart := time.Now()
	if sp, ok := client.(sparseFilePutter); ok {
		if f, isFile := content.(*os.File); isFile {
			stats, err := sp.PutSparseFile(ctx, bucket, object, f)
			switch {
			case err == nil:
				slog.InfoContext(ctx, "Compressed zstd upload",
					slog.String("object", object), slog.Bool("sparse", true),
					slog.Bool("ranged", true),
					slog.Int64("logical_bytes", stats.LogicalBytes),
					slog.Int64("populated_bytes", stats.PopulatedBytes),
					slog.Int64("wire_bytes", stats.WireBytes),
					slog.Duration("total", time.Since(tStart)))
				return stats, nil
			case !errors.Is(err, errSparseTooSmall):
				return UploadStats{}, fmt.Errorf("while putting object %q: %w", object, err)
			}
			// Too small to split: fall through to the streaming path.
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return UploadStats{}, err
			}
		}
	}
	if _, ok := client.(streamingPutter); ok {
		return sendStreamingZstd(ctx, client, bucket, object, content, tStart)
	}
	return sendBufferedZstd(ctx, client, bucket, object, content, tStart)
}

// sendBufferedZstd compresses content to a seekable temp file, then uploads it.
// Used for backends (S3/rustfs) whose PutObject needs a seekable body to sign and
// set Content-Length; the streaming counterpart is sendStreamingZstd.
func sendBufferedZstd(ctx context.Context, client ObjectStorage, bucket, object string, content io.Reader, tStart time.Time) (UploadStats, error) {
	tmpFile, err := os.CreateTemp("", "substrate-upload-compress-")
	if err != nil {
		return UploadStats{}, fmt.Errorf("while creating temp compress file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	t0 := time.Now()
	res, err := writeContent(tmpFile, content)
	if err != nil {
		return UploadStats{}, fmt.Errorf("while compressing %q: %w", object, err)
	}
	dCompress := time.Since(t0)

	// The temp file holds exactly the compressed payload, so its size is the
	// wire byte count.
	fi, err := tmpFile.Stat()
	if err != nil {
		return UploadStats{}, fmt.Errorf("while sizing temp file: %w", err)
	}
	stats := UploadStats{
		LogicalBytes:   res.logicalBytes,
		PopulatedBytes: res.populatedBytes,
		WireBytes:      fi.Size(),
		Sparse:         res.sparse,
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return UploadStats{}, fmt.Errorf("while seeking temp file: %w", err)
	}
	if err := client.PutObject(ctx, bucket, object, tmpFile); err != nil {
		return UploadStats{}, fmt.Errorf("while putting object %q: %w", object, err)
	}
	slog.InfoContext(ctx, "Compressed zstd upload",
		slog.String("object", object), slog.Bool("sparse", stats.Sparse),
		slog.Int64("logical_bytes", stats.LogicalBytes), slog.Int64("populated_bytes", stats.PopulatedBytes),
		slog.Int64("wire_bytes", stats.WireBytes),
		slog.Duration("compress", dCompress), slog.Duration("total", time.Since(tStart)))
	return stats, nil
}

// sendStreamingZstd compresses content and uploads it in one overlapped pass: a
// goroutine writes the (sparse-extent or plain) zstd stream into an io.Pipe while
// PutObject streams the read end to the object store. No seekable temp file, and
// the compress runs concurrently with the network PUT. Used only for streaming
// backends (GCS); see sendZstd.
func sendStreamingZstd(ctx context.Context, client ObjectStorage, bucket, object string, content io.Reader, tStart time.Time) (UploadStats, error) {
	type result struct {
		res writeContentResult
		err error
	}
	pr, pw := io.Pipe()
	ch := make(chan result, 1)
	// Everything the compressor feeds the pipe is what PutObject sends: count
	// it on the write side, where the byte count survives a failed upload.
	var wire atomic.Int64
	go func() {
		res, err := writeContent(countingWriter{w: pw, n: &wire}, content)
		// Closing the writer delivers EOF (or the compress error) to PutObject.
		_ = pw.CloseWithError(err)
		ch <- result{res: res, err: err}
	}()

	putErr := client.PutObject(ctx, bucket, object, pr)
	if putErr != nil {
		// PutObject bailed (e.g. mid-stream); unblock the compressor goroutine so it
		// can finish and we don't deadlock on the channel receive below.
		_ = pr.CloseWithError(putErr)
	}
	r := <-ch
	if putErr != nil {
		return UploadStats{}, fmt.Errorf("while putting object %q: %w", object, putErr)
	}
	if r.err != nil {
		return UploadStats{}, fmt.Errorf("while compressing %q: %w", object, r.err)
	}
	stats := UploadStats{
		LogicalBytes:   r.res.logicalBytes,
		PopulatedBytes: r.res.populatedBytes,
		WireBytes:      wire.Load(),
		Sparse:         r.res.sparse,
	}
	slog.InfoContext(ctx, "Compressed zstd upload",
		slog.String("object", object), slog.Bool("sparse", stats.Sparse), slog.Bool("streaming", true),
		slog.Int64("logical_bytes", stats.LogicalBytes), slog.Int64("populated_bytes", stats.PopulatedBytes),
		slog.Int64("wire_bytes", stats.WireBytes),
		slog.Duration("total", time.Since(tStart)))
	return stats, nil
}

// plainZstd writes src to w as a single plain zstd stream (SpeedFastest, all
// cores) and returns the uncompressed byte count.
func plainZstd(w io.Writer, src io.Reader) (int64, error) {
	zw, err := zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(zw, src)
	if err != nil {
		zw.Close()
		return n, err
	}
	return n, zw.Close()
}

func FetchLocalFileFromGCSWithZstd(ctx context.Context, client ObjectStorage, gsURL string, localFilePath string) (stats DownloadStats, err error) {
	ctx, span := tracer.Start(ctx, "fetchLocalFileFromGCSWithZstd")
	defer span.End()

	localFile, err := os.Create(localFilePath)
	if err != nil {
		return DownloadStats{}, fmt.Errorf("while opening %q: %w", localFilePath, err)
	}
	defer func() {
		if closeErr := localFile.Close(); closeErr != nil {
			if err == nil {
				err = closeErr
			} else {
				slog.InfoContext(ctx, "Dropped error from closing localFile", slog.String("localFile", localFilePath), slog.Any("err", err))
			}
		}
	}()

	if err := localFile.Chmod(0o600); err != nil {
		return DownloadStats{}, fmt.Errorf("in localFile.Chmod(0o600): %w", err)
	}

	stats, err = fetchFromGCSWithZstd(ctx, client, gsURL, localFile)
	if err != nil {
		return DownloadStats{}, fmt.Errorf("while fetching %q from GCS: %w", gsURL, err)
	}

	return stats, nil
}

func fetchFromGCSWithZstd(ctx context.Context, client ObjectStorage, gsURL string, out io.Writer) (stats DownloadStats, err error) {
	bucket, object, err := parseGCSURL(gsURL)
	if err != nil {
		return DownloadStats{}, fmt.Errorf("%w:while parsing URL: %w", ateerrors.ReasonInvalidObjectURL, err)
	}

	rc, err := client.GetObject(ctx, bucket, object)
	if err != nil {
		return DownloadStats{}, fmt.Errorf("while getting object: %w", err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			if err != nil {
				err = closeErr
			} else {
				slog.InfoContext(ctx, "Dropped error from rc.Close", slog.Any("err", closeErr))
			}
		}
	}()

	t0 := time.Now()
	// The decoder consumes the whole object, so the bytes read off rc are the
	// compressed (wire) byte count.
	cr := &countingReader{r: rc}
	res, err := decodeContent(out, cr)
	if err != nil {
		return DownloadStats{}, err
	}
	stats = DownloadStats{
		LogicalBytes:   res.logicalBytes,
		PopulatedBytes: res.writtenBytes,
		WireBytes:      cr.n,
		Sparse:         res.sparse,
	}
	slog.InfoContext(ctx, "Decompressed zstd download",
		slog.Bool("sparse", stats.Sparse), slog.Int64("logical_bytes", stats.LogicalBytes),
		slog.Int64("written_bytes", stats.PopulatedBytes), slog.Int64("wire_bytes", stats.WireBytes),
		slog.Duration("took", time.Since(t0)))
	return stats, nil
}

// decodeContentResult reports what decodeContent decompressed.
type decodeContentResult struct {
	// logicalBytes is the logical size written to out (the original image size).
	logicalBytes int64
	// writtenBytes is the count of non-hole bytes actually written on the
	// file-destination paths; 0 on the io.Copy fallback (non-file destination).
	writtenBytes int64
	// sparse is true when the input used the sparse-extent format.
	sparse bool
}

// decodeContent decompresses src into out, auto-detecting the format from the
// leading magic: the sparse-extent format (sparseMagic) vs a plain zstd stream
// (older snapshots, or the non-file upload path). When out is an *os.File the plain
// path writes SPARSE (skips zero blocks → holes) so only the resident set is
// written, not a dense multi-GiB image. It touches only io, so it is unit-testable
// without an object store, mirroring writeContent.
func decodeContent(out io.Writer, src io.Reader) (decodeContentResult, error) {
	magic := make([]byte, len(sparseMagic))
	n, rerr := io.ReadFull(src, magic)
	if rerr == nil && string(magic) == sparseMagic {
		f, ok := out.(*os.File)
		if !ok {
			return decodeContentResult{}, fmt.Errorf("sparse-extent snapshot requires a file destination, got %T", out)
		}
		size, written, derr := readSparseZstd(f, src) // src is positioned just after the magic
		if derr != nil {
			return decodeContentResult{}, fmt.Errorf("in sparse-extent decode: %w", derr)
		}
		return decodeContentResult{logicalBytes: size, writtenBytes: written, sparse: true}, nil
	}
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		return decodeContentResult{}, fmt.Errorf("while reading object header: %w", rerr)
	}

	// Plain zstd stream: put back the peeked bytes, then decompress.
	r := io.MultiReader(bytes.NewReader(magic[:n]), src)
	zrc, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return decodeContentResult{}, fmt.Errorf("in zstd.NewReader: %w", err)
	}
	defer zrc.Close()
	if f, ok := out.(*os.File); ok {
		size, written, derr := copyZstdSparse(f, zrc)
		if derr != nil {
			return decodeContentResult{}, fmt.Errorf("in sparse decompress: %w", derr)
		}
		return decodeContentResult{logicalBytes: size, writtenBytes: written}, nil
	}
	size, cerr := io.Copy(out, zrc)
	if cerr != nil {
		return decodeContentResult{}, fmt.Errorf("in io.Copy: %w", cerr)
	}
	return decodeContentResult{logicalBytes: size}, nil
}

// copyZstdSparse writes src into dst skipping all-zero blocks, so dst becomes a
// sparse file (the skipped regions are holes). Returns the logical size (total bytes
// consumed from src) and the bytes actually written (non-zero). dst is truncated to
// empty first (so skipped regions are real holes, not stale bytes) and to the
// logical size at the end (so trailing zero regions become a hole and the size is
// exact). dst must be a regular file opened for writing.
func copyZstdSparse(dst *os.File, src io.Reader) (size int64, written int64, err error) {
	// Start from an empty file so the holes we skip can't expose pre-existing bytes:
	// this writes out only the non-zero chunks, it does not overlay onto dst.
	if err := dst.Truncate(0); err != nil {
		return 0, 0, fmt.Errorf("truncating dst: %w", err)
	}
	// 64KiB blocks: a multiple of the 4KiB fs block (so skipped runs align to whole
	// hole-able blocks) while keeping the zero-scan + WriteAt syscall count modest.
	const block = 64 << 10
	buf := make([]byte, block)
	var pos int64
	for {
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			chunk := buf[:n]
			if !allZero(chunk) {
				if _, werr := dst.WriteAt(chunk, pos); werr != nil {
					return 0, 0, werr
				}
				written += int64(n)
			}
			pos += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return 0, 0, rerr
		}
	}
	// Materialize the exact logical size: extends past the last written byte with a
	// hole when the tail was zero (skipped), and is a no-op otherwise.
	if terr := dst.Truncate(pos); terr != nil {
		return 0, 0, terr
	}
	return pos, written, nil
}

// allZero reports whether b is all zero bytes, checking 8 bytes at a time.
func allZero(b []byte) bool {
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if binary.LittleEndian.Uint64(b[i:]) != 0 {
			return false
		}
	}
	for ; i < len(b); i++ {
		if b[i] != 0 {
			return false
		}
	}
	return true
}

func parseGCSURL(gsURL string) (string, string, error) {
	parsed, err := url.Parse(gsURL)
	if err != nil {
		return "", "", fmt.Errorf("while parsing %q: %w", gsURL, err)
	}

	return parsed.Host, strings.TrimPrefix(parsed.Path, "/"), nil
}
