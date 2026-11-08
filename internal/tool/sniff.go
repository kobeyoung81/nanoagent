package tool

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// sniffSampleBytes bounds how much of a file the type detector inspects.
const sniffSampleBytes = 8 << 10

// contentTypeSniffBytes is how much http.DetectContentType itself looks at.
const contentTypeSniffBytes = 512

// badUTF8Threshold is the fraction of undecodable bytes above which a sample is
// called binary.
const badUTF8Threshold = 0.10

// contentKind is the verdict of sniffing the head of a file.
type contentKind struct {
	// Sniffed is the content type http.DetectContentType inferred.
	Sniffed string
	// Binary reports that the content must not be handed to the model as text.
	Binary bool
	// Reason is a short phrase explaining a Binary verdict.
	Reason string
	// Encoding is the detected text encoding ("" = UTF-8/unknown, "utf-8" = BOM).
	Encoding string
}

// binaryContentPrefixes are the sniffed types that always mean "not text".
var binaryContentPrefixes = []string{
	"image/", "audio/", "video/", "application/pdf", "application/zip",
	"application/gzip", "application/x-tar", "application/x-rar",
	"application/x-7z-compressed", "application/x-bzip", "application/octet-stream",
}

// textExtensions are extensions that promise text, so a binary body behind one
// of them is worth reporting as a mismatch.
var textExtensions = map[string]bool{
	"md": true, "markdown": true, "txt": true, "text": true, "log": true,
	"go": true, "mod": true, "sum": true, "ts": true, "tsx": true, "js": true,
	"jsx": true, "mjs": true, "cjs": true, "json": true, "jsonl": true,
	"yaml": true, "yml": true, "toml": true, "ini": true, "conf": true,
	"cfg": true, "env": true, "csv": true, "tsv": true, "sql": true,
	"sh": true, "bash": true, "py": true, "rb": true, "rs": true, "java": true,
	"c": true, "h": true, "cc": true, "cpp": true, "hpp": true, "css": true,
	"scss": true, "html": true, "htm": true, "xml": true, "svg": true,
	"diff": true, "patch": true, "pl": true, "lua": true, "php": true,
}

// binaryExtensions are extensions that promise binary content, so text behind
// one of them is worth reporting too.
var binaryExtensions = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true,
	"bmp": true, "ico": true, "pdf": true, "zip": true, "gz": true, "tgz": true,
	"bz2": true, "xz": true, "7z": true, "rar": true, "tar": true,
	"woff": true, "woff2": true, "ttf": true, "otf": true, "so": true,
	"o": true, "a": true, "exe": true, "bin": true, "class": true, "jar": true,
	"mp3": true, "mp4": true, "mov": true, "avi": true, "wav": true,
	"sqlite": true, "db": true, "wasm": true,
}

// sniffContent classifies the head of a file. Encoding is resolved first: a
// UTF-16/32 document is full of NUL bytes by construction, and calling it binary
// because of that would be exactly the misjudgement this helper exists to
// prevent.
func sniffContent(sample []byte) contentKind {
	kind := contentKind{Sniffed: contentTypeOf(sample)}
	if enc := bomEncoding(sample); enc != "" {
		kind.Encoding = enc
		if enc != "utf-8" {
			return kind
		}
		sample = sample[3:]
	}
	if isBinaryContentType(kind.Sniffed) {
		kind.Binary, kind.Reason = true, "binary content type "+kind.Sniffed
		return kind
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		kind.Binary, kind.Reason = true, "contains NUL bytes"
		return kind
	}
	if badUTF8Ratio(sample) > badUTF8Threshold {
		kind.Binary, kind.Reason = true, "not valid UTF-8 text"
	}
	return kind
}

// contentTypeOf runs the standard-library sniffer over the sample.
func contentTypeOf(sample []byte) string {
	if len(sample) > contentTypeSniffBytes {
		sample = sample[:contentTypeSniffBytes]
	}
	return http.DetectContentType(sample)
}

// isBinaryContentType reports whether a sniffed type always means binary.
func isBinaryContentType(ct string) bool {
	for _, prefix := range binaryContentPrefixes {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

// badUTF8Ratio returns the fraction of the sample that is not valid UTF-8.
func badUTF8Ratio(sample []byte) float64 {
	if len(sample) == 0 {
		return 0
	}
	bad, total := 0, 0
	for len(sample) > 0 {
		r, size := utf8.DecodeRune(sample)
		total++
		if r == utf8.RuneError && size == 1 {
			bad++
		}
		sample = sample[size:]
	}
	return float64(bad) / float64(total)
}

// bomEncoding returns the encoding named by a byte-order mark ("" when absent).
func bomEncoding(s []byte) string {
	switch {
	case len(s) >= 4 && s[0] == 0xFF && s[1] == 0xFE && s[2] == 0x00 && s[3] == 0x00:
		return "utf-32le"
	case len(s) >= 4 && s[0] == 0x00 && s[1] == 0x00 && s[2] == 0xFE && s[3] == 0xFF:
		return "utf-32be"
	case len(s) >= 2 && s[0] == 0xFF && s[1] == 0xFE:
		return "utf-16le"
	case len(s) >= 2 && s[0] == 0xFE && s[1] == 0xFF:
		return "utf-16be"
	case len(s) >= 3 && s[0] == 0xEF && s[1] == 0xBB && s[2] == 0xBF:
		return "utf-8"
	}
	return ""
}

// decodeEncoded converts a UTF-16/32 document named by its BOM into UTF-8 text.
// Undecodable units become U+FFFD instead of failing the whole read.
func decodeEncoded(data []byte, encoding string) string {
	switch encoding {
	case "utf-16le", "utf-16be":
		big := encoding == "utf-16be"
		body := data[2:]
		var b strings.Builder
		b.Grow(len(body))
		for i := 0; i+1 < len(body); i += 2 {
			unit := binary.LittleEndian.Uint16(body[i:])
			if big {
				unit = binary.BigEndian.Uint16(body[i:])
			}
			switch {
			case unit >= 0xD800 && unit <= 0xDBFF: // high surrogate
				if i+3 < len(body) {
					next := binary.LittleEndian.Uint16(body[i+2:])
					if big {
						next = binary.BigEndian.Uint16(body[i+2:])
					}
					if next >= 0xDC00 && next <= 0xDFFF {
						r := rune(unit-0xD800)<<10 | rune(next-0xDC00) + 0x10000
						b.WriteRune(r)
						i += 2
						continue
					}
				}
				b.WriteRune(utf8.RuneError)
			case unit >= 0xDC00 && unit <= 0xDFFF: // lone low surrogate
				b.WriteRune(utf8.RuneError)
			default:
				b.WriteRune(rune(unit))
			}
		}
		return b.String()
	case "utf-32le", "utf-32be":
		big := encoding == "utf-32be"
		body := data[4:]
		var b strings.Builder
		b.Grow(len(body) / 2)
		for i := 0; i+3 < len(body); i += 4 {
			v := binary.LittleEndian.Uint32(body[i:])
			if big {
				v = binary.BigEndian.Uint32(body[i:])
			}
			if v > utf8.MaxRune || (v >= 0xD800 && v <= 0xDFFF) {
				b.WriteRune(utf8.RuneError)
				continue
			}
			b.WriteRune(rune(v))
		}
		return b.String()
	}
	return string(data)
}

// mismatchNote describes an extension that does not match the content, so the
// model is told which of the two it is actually looking at instead of guessing.
func mismatchNote(path string, kind contentKind) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return ""
	}
	switch {
	case kind.Binary && textExtensions[ext]:
		return "extension ." + ext + " says text but the content is " + kind.Reason
	case !kind.Binary && binaryExtensions[ext]:
		return "extension ." + ext + " says binary but the content is text (" + kind.Sniffed + ")"
	}
	return ""
}
